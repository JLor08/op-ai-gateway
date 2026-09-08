// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import { Tooltip } from '@mui/material';
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
// lifecycle. Only the running and the LOADING states get their own treatment;
// every other explicit state (stopped/draining/backoff/crashed/...) collapses
// to "not loaded" here (its detail, if ever needed, belongs elsewhere, not a
// fourth chip colour on this screen). `""` -- no agent-managed runtime status
// at all, e.g. a non-server_agent model server -- falls back to the
// benchmark-derived `loaded` boolean, exactly what the old "Geladen" column
// showed on its own.
type ModelStatusKey = 'loaded' | 'loading' | 'not_loaded';

// The runtime states that mean "on its way up", i.e. the user-visible "Lädt".
// This is NOT just `starting`: the shared `runtimeStateBadge`
// (shared/runtimeState.ts) maps `starting` AND `pending_vram_unknown` onto the
// same `watch` badge, because both are "waiting to be loaded". This column
// claims to reuse that vocabulary, so it must agree with it -- a
// `pending_vram_unknown` spec that reads yellow "Lädt" on the runtime admin
// screen must not read grey "Nicht Geladen" here for the same instant of the
// same spec.
const loadingRuntimeStates = new Set(['starting', 'pending_vram_unknown']);

function modelStatusKey(r: Pick<ModelServerRow, 'state' | 'loaded'>): ModelStatusKey {
  if (r.state === 'running') return 'loaded';
  if (loadingRuntimeStates.has(r.state)) return 'loading';
  if (r.state === '') return r.loaded ? 'loaded' : 'not_loaded';
  return 'not_loaded';
}

// Colour: the running + loading states reuse the SAME runtimeStateBadge
// mapping RuntimeAdminSection's "Live status" column uses (active/watch), so
// this column's colours never drift from that vocabulary; every other case
// only ever needs the coarse loaded ("success", same visual class as "active")
// / not-loaded ("standby") distinction the old "Geladen" column already had.
function modelStatusBadge(r: Pick<ModelServerRow, 'state' | 'loaded'>): BadgeStatus {
  if (r.state === 'running' || loadingRuntimeStates.has(r.state)) return runtimeStateBadge(r.state);
  return modelStatusKey(r) === 'loaded' ? 'success' : 'standby';
}

function modelStatusLabel(key: ModelStatusKey, t: Translation): string {
  if (key === 'loaded') return t.tableModelLoaded;
  if (key === 'loading') return t.modelServerLoading;
  return t.modelServerNotLoaded;
}

// Task 7: `metrics_probe` is "ok" | "unreachable" | "na" | "" (api/models.ts)
// -- ONLY "ok" means the accompanying numbers (active_requests/queue_depth)
// are a real measurement rather than an unset zero, because those two are
// PROBE-DERIVED: the portal service leaves them at 0 and only the
// capability-gated `injectRowRuntimeState` ever fills them. One tiny shared
// predicate so both gated columns below agree on what "measured" means.
//
// Deliberately NOT used for the Kontext column: `context_size` is a persisted
// mapping field, not a probe result -- see that column's own comment.
function probeOk(probeState: string): boolean {
  return probeState === 'ok';
}

// `live_progress_support` is the mapping's PERSISTED verdict on whether this
// upstream tolerates the live-progress two-parameter request (#51):
// "supported" / "unsupported" / "" (never determined). Deliberately NOT
// gated on a probe field, for exactly the reason the Kontext column's own
// comment above gives: the portal service fills it once from the PERSISTED
// mapping field (`LiveProgressSupport: view.mapping.LiveProgressSupport`,
// service_model_servers.go), which a background detector writes directly to
// the mapping (routing.Store.UpdateMappingLiveProgressSupport) -- there is
// no gateway-injection seam to gate this on, unlike metrics_probe/
// context_probe/state/active_requests/queue_depth above. Gating this cell on
// a probe field would repeat the exact bug the Kontext column's fix already
// closed: hiding a real, persisted verdict on every row a probe fixture
// doesn't happen to reach.
//
// The design spec calls for rendering NOTHING when this is "". That is
// DELIBERATELY OVERRIDDEN here: this codebase already has a convention for
// "the field exists, nothing has been measured/determined yet" -- the shared
// "—" placeholder the active/queue/context columns above use -- and an
// indistinguishable "column doesn't exist" vs "nothing determined yet" has
// already cost a real support report on a sibling table's silent `null`. So
// "" renders "—" here too (see the render callback below), not an empty
// cell.
function liveProgressChipInfo(
  support: string,
  t: Translation,
): { status: BadgeStatus; label: string; tooltip: string } | undefined {
  if (support === 'supported') {
    return {
      status: 'success',
      label: t.modelServerLiveProgressSupported,
      tooltip: t.modelServerLiveProgressTooltipSupported,
    };
  }
  if (support === 'unsupported') {
    // The NEUTRAL badge, deliberately NOT the attention/"watch" one: an older
    // llama.cpp build that lacks this request parameter is not broken, it
    // simply lacks a nicety. Flagging it as attention-worthy would put a
    // warning on every such server -- the same mistake as showing a bare 0
    // that actually means "not measured".
    return {
      status: 'standby',
      label: t.modelServerLiveProgressUnsupported,
      tooltip: t.modelServerLiveProgressTooltipUnsupported,
    };
  }
  return undefined;
}

// live_progress_checked_at exists ONLY for this tooltip and for operator
// diagnostics (mirrors ModelMapping.LiveProgressCheckedAt's own doc-comment)
// -- it is folded into the tooltip string here, not read by any rendering
// decision (the badge/colour choice above depends solely on
// live_progress_support).
function liveProgressTooltip(
  info: { tooltip: string },
  checkedAt: string | null | undefined,
  t: Translation,
): string {
  if (!checkedAt) return info.tooltip;
  return `${info.tooltip} ${t.modelServerLiveProgressCheckedAt(new Date(checkedAt).toLocaleString())}`;
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
      // Gated on the VALUE (> 0), not on `context_probe` -- deliberately
      // unlike active/queue above, and this is the difference that matters:
      // `context_size` is NOT probe-derived. The portal service fills it once
      // from the PERSISTED mapping field (`ContextSize: view.mapping.
      // ContextSize`, portal/service_model_servers.go), and that field can
      // come from a benchmark run or a manual operator entry just as well as
      // from the agent's context probe. Gating the cell on the live probe
      // state therefore hid a real, stored context size on every non-probing
      // row -- any non-`server_agent` model server, and any agent without the
      // runtime_model_probe capability. `context_probe` reports only whether
      // the probe that can REFRESH this value is currently reachable, which is
      // what the runtime admin screen's "Probes" column shows; it says nothing
      // about whether the stored value is real. A real context size of 0
      // cannot happen, so "> 0" is exactly the "nothing is known" test and the
      // feature's intent (never render a meaningless 0 as 0) still holds.
      id: 'context',
      label: t.mappingContextSize,
      numeric: true,
      value: (r) => String(r.context_size),
      render: (r) => (r.context_size > 0 ? String(r.context_size) : '—'),
    },
    {
      // See liveProgressChipInfo above for why this is read straight off the
      // row's persisted field, not gated on any probe column.
      id: 'liveProgress',
      label: t.modelServerColLiveProgress,
      value: (r) => r.live_progress_support,
      filter: 'enum',
      searchable: false,
      // Two branches only: ListTable's enumOptions drops '' from the derived
      // option list, so a "never determined" row is never an option and no
      // third label is reachable from here. (Whether the column SHOULD offer a
      // "never determined" filter is a product question, not a rendering one.)
      // The '' rows are still rendered -- as the shared "—" placeholder, by
      // the `render` below -- they are just not filterable by that value.
      enumLabel: (v) =>
        v === 'supported'
          ? t.modelServerLiveProgressSupported
          : t.modelServerLiveProgressUnsupported,
      render: (r) => {
        const info = liveProgressChipInfo(r.live_progress_support, t);
        if (!info) return '—';
        return (
          <Tooltip title={liveProgressTooltip(info, r.live_progress_checked_at, t)}>
            <span>
              <StatusChip status={info.status} label={info.label} />
            </span>
          </Tooltip>
        );
      },
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
