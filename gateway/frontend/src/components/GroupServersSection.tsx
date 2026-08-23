// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import type { GroupModelServerRow, ModelOption } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { StatusChip } from './shared/StatusChip';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { makeVisionColumn } from './shared/visionColumn';

/**
 * Group-detail sub-view: every (model, server) a model GROUP can serve — its
 * flattened members — with each mapping's benchmark metrics and a LIVE "Prio"
 * rank across the whole group's candidate list. Read-only (no "Laden" action;
 * loading a specific model+server belongs to the per-model detail view).
 *
 * Live: there is no SSE for this endpoint, so a ~3s poll re-fetches the list —
 * mirroring the model-detail view's Prio poll. A fresh, full `rows` array each
 * tick preserves the user's search/filter/sort/column settings.
 */
export function GroupServersSection({
  t,
  api,
  group,
  // `isAdmin` is accepted for symmetry with the per-model detail view (and future
  // gating); this view is read-only regardless, so it's intentionally unused
  // here (renamed with the `_` prefix the lint config allows for that).
  isAdmin: _isAdmin,
  pollIntervalMs,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'modelGroupServers'>;
  group: ModelOption;
  isAdmin: boolean;
  // Poll cadence (ms); injectable so tests drive the loop without a real wait.
  pollIntervalMs?: number;
}>) {
  const [rows, setRows] = useState<GroupModelServerRow[]>([]);
  const [loading, setLoading] = useState(true);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    setLoading(true);
    api
      .modelGroupServers(group.id)
      .then((r) => {
        if (mountedRef.current) setRows(r);
      })
      .catch(() => {
        /* the empty table + the next poll tick still recover */
      })
      .finally(() => {
        if (mountedRef.current) setLoading(false);
      });
    return () => {
      mountedRef.current = false;
    };
  }, [api, group.id]);

  // Live re-ranking poll (no SSE for this endpoint): the backend recomputes each
  // row's `priority` continuously as load/telemetry shifts, so a ~3s poll keeps
  // the "Prio" column current. Feeding a fresh, full `rows` snapshot each tick
  // preserves the user's search/filter/sort/column settings.
  useEffect(() => {
    const id = setInterval(() => {
      api
        .modelGroupServers(group.id)
        .then((r) => {
          if (mountedRef.current) setRows(r);
        })
        .catch(() => {
          /* transient — the next tick recovers */
        });
    }, pollIntervalMs ?? 3000);
    return () => clearInterval(id);
  }, [api, group.id, pollIntervalMs]);

  const columns: ListColumn<GroupModelServerRow>[] = [
    {
      id: 'prio',
      label: t.modelServerColPrio,
      numeric: true,
      value: (r) => String(r.priority || 0),
      render: (r) => (r.priority > 0 ? String(r.priority) : '-'),
    },
    { id: 'model', label: t.modelServerColModel, value: (r) => r.model, filter: 'text' },
    { id: 'server', label: t.modelServerColServer, value: (r) => r.server_name, filter: 'text' },
    {
      id: 'loaded',
      label: t.tableModelLoaded,
      value: (r) => (r.loaded ? 'loaded' : 'unloaded'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'loaded' ? t.tableModelLoaded : t.modelServerNotLoaded),
      render: (r) =>
        r.loaded ? (
          <StatusChip status="success" label={t.tableModelLoaded} />
        ) : (
          <StatusChip status="standby" label={t.modelServerNotLoaded} />
        ),
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
      defaultHidden: true,
    },
    {
      id: 'context',
      label: t.mappingContextSize,
      numeric: true,
      value: (r) => String(r.context_size),
      render: (r) => (r.context_size > 0 ? String(r.context_size) : '-'),
      defaultHidden: true,
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

  return (
    <Panel
      titleId="group-servers-heading"
      title={t.modelServerTitle}
      subtitle={t.groupServersIntro}
    >
      <ListTable
        rows={rows}
        columns={columns}
        rowKey={(r) => r.model + '/' + r.mapping_id}
        storageKey="op.group-servers"
        labels={listTableLabels(t)}
        loading={loading}
      />
    </Panel>
  );
}
