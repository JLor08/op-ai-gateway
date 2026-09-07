// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import DownloadIcon from '@mui/icons-material/Download';
import { PortalApiError, type ModelOption, type ModelServerRow } from '../api';
import type { BadgeStatus, PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { StatusChip } from './shared/StatusChip';
import { runtimeStateBadge } from './shared/runtimeState';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { makeVisionColumn } from './shared/visionColumn';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { pollBenchmarkStatus } from './shared/benchmark';
import { formatPortalError } from './shared/format';

/**
 * Per-model detail sub-view: the servers offering a gateway model, each with its
 * mapping's benchmark metrics, a LIVE loaded indicator (fed by SSE), a LIVE "Prio"
 * rank (fed by a ~3s poll), a LIVE runtime-state badge + active/queue counts (also
 * SSE-fed, sharing the state vocabulary RuntimeAdminSection's "Live status" column
 * uses), and a "Laden" (load) row action gated on can_load / loaded / server-idle.
 * A full ListTable (search / filter / sort / columns), the same as every other
 * admin list.
 *
 * Live: on mount it fetches the offering list, then subscribes to the per-model SSE
 * (snapshot + update frames) AND starts a ~3s poll that re-fetches the same list so
 * the "Prio" column re-ranks as load/telemetry shifts server standing (the SSE only
 * fires on a loaded-state change). Feeding a fresh `rows` array from either source
 * preserves the user's search/filter/sort/column settings, so it is safe to setRows
 * on every update.
 */

// Task 7 (probe-reachability-and-model-status): the merged "Status" column's
// tri-state, replacing the old separate "Geladen" (benchmark-derived `loaded`)
// and "Live-Status" (raw runtime `state`) columns -- this column answers ONE
// question ("can I use it right now?"), not the full nine-value runtime
// lifecycle. Only `running` and `starting` get their own treatment; every
// other explicit state (stopped/draining/backoff/crashed/...) collapses to
// "not loaded" here (its detail, if ever needed, belongs elsewhere, not a
// fourth chip colour on this screen). `""` -- no agent-managed runtime status
// at all, e.g. a non-server_agent model server -- falls back to the
// benchmark-derived `loaded` boolean, exactly what the old "Geladen" column
// showed on its own.
type ModelStatusKey = 'loaded' | 'loading' | 'not_loaded';

function modelStatusKey(r: Pick<ModelServerRow, 'state' | 'loaded'>): ModelStatusKey {
  if (r.state === 'running') return 'loaded';
  if (r.state === 'starting') return 'loading';
  if (r.state === '') return r.loaded ? 'loaded' : 'not_loaded';
  return 'not_loaded';
}

// Colour: "running"/"starting" reuse the SAME runtimeStateBadge mapping
// RuntimeAdminSection's "Live status" column uses (active/watch), so this
// column's colours never drift from that vocabulary; every other case only
// ever needs the coarse loaded ("success", same visual class as "active") /
// not-loaded ("standby") distinction the old "Geladen" column already had.
function modelStatusBadge(r: Pick<ModelServerRow, 'state' | 'loaded'>): BadgeStatus {
  if (r.state === 'running' || r.state === 'starting') return runtimeStateBadge(r.state);
  return modelStatusKey(r) === 'loaded' ? 'success' : 'standby';
}

function modelStatusLabel(key: ModelStatusKey, t: Translation): string {
  if (key === 'loaded') return t.tableModelLoaded;
  if (key === 'loading') return t.modelServerLoading;
  return t.modelServerNotLoaded;
}

// Task 7: `metrics_probe`/`context_probe` are "ok" | "unreachable" | "na" | ""
// (api/models.ts) -- ONLY "ok" means the accompanying number (active_requests/
// queue_depth for metrics_probe, context_size for context_probe) is a real
// measurement rather than an unset zero. One tiny shared predicate so all
// three gated columns below agree on what "measured" means.
function probeOk(probeState: string): boolean {
  return probeState === 'ok';
}

export function ModelServersSection({
  t,
  api,
  model,
  // `isAdmin` is accepted for symmetry with the other list views (and future
  // gating); the backend already enforces can_load per row, so it's
  // intentionally unused here (renamed with the `_` prefix the lint config
  // allows for that).
  isAdmin: _isAdmin,
  pollIntervalMs,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'benchmarkStatus' | 'loadModel' | 'modelServers' | 'subscribeModelServers'>;
  model: ModelOption;
  isAdmin: boolean;
  // Load-completion poll cadence (ms); injectable so tests drive the loop without a
  // real 2s wait. Defaults to the shared helper's cadence.
  pollIntervalMs?: number;
}>) {
  const { showError, showSuccess } = useToast();
  const [rows, setRows] = useState<ModelServerRow[]>([]);
  const [loading, setLoading] = useState(true);
  // Per-mapping in-flight guard: while a load is running we disable that row's
  // "Laden" and show a "Lädt…" hint.
  const [inFlight, setInFlight] = useState<Record<string, boolean>>({});
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    setLoading(true);
    api
      .modelServers(model.id)
      .then((r) => {
        if (mountedRef.current) setRows(r);
      })
      .catch(() => {
        /* the empty table + the SSE subscription still recover */
      })
      .finally(() => {
        if (mountedRef.current) setLoading(false);
      });
    const unsub = api.subscribeModelServers(model.id, (r) => {
      if (mountedRef.current) setRows(r);
    });
    return () => {
      mountedRef.current = false;
      unsub();
    };
  }, [api, model.id]);

  // Live re-ranking poll: the backend recomputes each row's `priority` (1-based rank)
  // continuously as load/telemetry shifts, so a ~3s poll keeps the "Prio" column
  // current independent of the loaded-state SSE (which only fires on a load change).
  // Feeding a fresh, full `rows` snapshot each tick preserves the user's
  // search/filter/sort/column settings, same as the SSE frames above.
  useEffect(() => {
    const id = setInterval(() => {
      api
        .modelServers(model.id)
        .then((r) => {
          if (mountedRef.current) setRows(r);
        })
        .catch(() => {
          /* transient — the next tick or the SSE recovers */
        });
    }, pollIntervalMs ?? 3000);
    return () => clearInterval(id);
  }, [api, model.id, pollIntervalMs]);

  // Load a mapping's model on its server (idle-gated backend). Poll to completion,
  // then surface success / the run's error / a specific 409 toast.
  async function doLoad(row: ModelServerRow) {
    setInFlight((p) => ({ ...p, [row.mapping_id]: true }));
    try {
      await api.loadModel(row.mapping_id);
      showSuccess(t.modelServerLoadStarted);
      const status = await pollBenchmarkStatus(api, row.server_id, { intervalMs: pollIntervalMs });
      const err =
        (status.results ?? []).find((r) => r.mapping_id === row.mapping_id)?.error ?? status.error;
      if (err) showError(t.modelServerLoadError);
      else showSuccess(t.modelServerLoadSuccess);
    } catch (e) {
      // A 409 = the server is busy / a run is already on it → a specific toast; any
      // other error → the shared formatted message.
      const code = e instanceof PortalApiError ? e.code : '';
      if (code === 'benchmark.server_in_use') showError(t.modelServerBusy);
      else if (code === 'benchmark.already_running') showError(t.modelServerAlreadyRunning);
      else showError(formatPortalError(e, t));
    } finally {
      if (mountedRef.current) setInFlight((p) => ({ ...p, [row.mapping_id]: false }));
    }
  }

  const columns: ListColumn<ModelServerRow>[] = [
    {
      id: 'prio',
      label: t.modelServerColPrio,
      numeric: true,
      value: (r) => String(r.priority || 0),
      render: (r) => (r.priority > 0 ? String(r.priority) : '-'),
    },
    { id: 'server', label: t.modelServerColServer, value: (r) => r.server_name, filter: 'text' },
    {
      // The merged tri-state status (SSE-fed, same source as the old separate
      // "Geladen" + "Live-Status" columns it replaces) — see modelStatusKey
      // above for the exact running/starting/fallback rules.
      id: 'status',
      label: t.modelServerColStatus,
      value: (r) => modelStatusKey(r),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => modelStatusLabel(v as ModelStatusKey, t),
      render: (r) => (
        <StatusChip status={modelStatusBadge(r)} label={modelStatusLabel(modelStatusKey(r), t)} />
      ),
    },
    {
      // Live per-model load (SSE-fed): how many requests are in flight on this
      // (server, mapping) right now. 0 is a real, meaningful value (idle) —
      // but ONLY when metrics_probe actually reached the agent; otherwise this
      // number was never measured and rendering it (even as 0) would be a
      // misleading placeholder, so it renders "—" (the portal's shared "never
      // measured" glyph, e.g. shared/format.ts) instead.
      id: 'active',
      label: t.modelServerColActive,
      numeric: true,
      value: (r) => String(r.active_requests),
      render: (r) => (probeOk(r.metrics_probe) ? String(r.active_requests) : '—'),
    },
    {
      // Live per-model load (SSE-fed): how many requests are waiting for
      // admission on this (server, mapping) right now. Same "0 is real, but
      // only when measured" rule as `active` above.
      id: 'queue',
      label: t.modelServerColQueue,
      numeric: true,
      value: (r) => String(r.queue_depth),
      render: (r) => (probeOk(r.metrics_probe) ? String(r.queue_depth) : '—'),
    },
    {
      id: 'genTps',
      label: t.mappingGenTokensPerSecond,
      numeric: true,
      value: (r) => String(r.gen_tokens_per_second),
      render: (r) => (r.gen_tokens_per_second > 0 ? r.gen_tokens_per_second.toFixed(1) : '-'),
    },
    {
      id: 'promptTps',
      label: t.mappingPromptTokensPerSecond,
      numeric: true,
      value: (r) => String(r.prompt_tokens_per_second),
      render: (r) => (r.prompt_tokens_per_second > 0 ? r.prompt_tokens_per_second.toFixed(1) : '-'),
    },
    {
      id: 'loadTime',
      label: t.mappingLoadTimeMs,
      numeric: true,
      value: (r) => String(r.load_time_ms),
      render: (r) => (r.load_time_ms > 0 ? String(r.load_time_ms) : '-'),
    },
    {
      // Gated on context_probe rather than "> 0": a real context size of 0
      // cannot happen, but gating on the probe state (not just the number)
      // keeps this column consistent with active/queue above and makes the
      // "never measured" case explicit rather than incidental.
      id: 'context',
      label: t.mappingContextSize,
      numeric: true,
      value: (r) => String(r.context_size),
      render: (r) => (probeOk(r.context_probe) ? String(r.context_size) : '—'),
    },
    {
      id: 'maxConc',
      label: t.mappingMaxConcurrency,
      numeric: true,
      value: (r) => String(r.max_concurrency),
      render: (r) => (r.max_concurrency > 0 ? String(r.max_concurrency) : '-'),
      defaultHidden: true,
    },
    {
      id: 'recConc',
      label: t.mappingRecommendedConcurrency,
      numeric: true,
      value: (r) => String(r.recommended_concurrency),
      render: (r) => (r.recommended_concurrency > 0 ? String(r.recommended_concurrency) : '-'),
      defaultHidden: true,
    },
    {
      id: 'genTpsCap',
      label: t.mappingGenTpsAtCapacity,
      numeric: true,
      value: (r) => String(r.gen_tokens_per_second_at_capacity),
      render: (r) =>
        r.gen_tokens_per_second_at_capacity > 0
          ? r.gen_tokens_per_second_at_capacity.toFixed(1)
          : '-',
      defaultHidden: true,
    },
    {
      id: 'mtp',
      label: t.mappingIsMtp,
      value: (r) => (r.is_mtp ? 'yes' : 'no'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'yes' ? t.yes : t.no),
      render: (r) => (r.is_mtp ? t.yes : '-'),
      defaultHidden: true,
    },
    makeVisionColumn(t, (r) => !!r.vision_capable, { defaultHidden: true }),
    {
      id: 'source',
      label: t.modelServerSource,
      value: (r) => r.metrics_source || '-',
      filter: 'enum',
      defaultHidden: true,
    },
    {
      id: 'updated',
      label: t.modelServerUpdated,
      value: (r) => r.metrics_updated_at ?? '',
      searchable: false,
      render: (r) => (r.metrics_updated_at ? new Date(r.metrics_updated_at).toLocaleString() : '-'),
      defaultHidden: true,
    },
  ];

  const rowActions = (r: ModelServerRow): RowAction[] => {
    let reason: string | undefined;
    if (!r.can_load) {
      reason = t.modelServerLoadDisabledPerm;
    } else if (r.loaded) {
      reason = t.modelServerLoadDisabledLoaded;
    } else if (inFlight[r.mapping_id]) {
      reason = t.modelServerLoadDisabledBusy;
    }
    return [
      {
        key: 'load',
        label: t.modelServerLoad,
        icon: <DownloadIcon fontSize="small" />,
        onClick: () => void doLoad(r),
        disabled: reason !== undefined,
        title: reason,
      },
    ];
  };

  return (
    <Panel titleId="model-servers-heading" title={t.modelServerTitle}>
      <ListTable
        rows={rows}
        columns={columns}
        rowKey={(r) => r.mapping_id}
        actions={rowActions}
        // Force the single "Laden" action into the kebab (⋮) row menu (not the
        // inline IconAction path, which drops a disabled action's `title`): the
        // menu renders a disabled item's reason via Tooltip+span, so an
        // already-loaded / not-owner row shows WHY "Laden" is disabled.
        maxInlineActions={0}
        storageKey="op.model-servers"
        labels={listTableLabels(t)}
        loading={loading}
      />
    </Panel>
  );
}
