// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationStatus, PortalModelMapping } from '../../api';
import type { ListColumn } from './ListTable';
import type { Translation } from './types';
import { formatMetric } from './format';
import { StatusChip } from './StatusChip';
import { makeVisionColumn } from './visionColumn';
import { applicationStatusLabelByKey } from './application';

/**
 * The model-mapping table, defined ONCE for the two screens that show it.
 *
 * `MappingSection` shows it for an ordinary application; `RuntimeAdminSection`'s
 * "model mapping" tab shows it for a `server_agent` application, where the same
 * rows are the routes whose launch specs live one tab to the right. The
 * requirement is literally "the same table", so the columns are one definition
 * rather than two that happen to agree -- the drift this prevents is silent
 * (a metric column added to one copy is invisible, no test and no type sees it).
 *
 * Pure data derived from `t`: no state, no api, no handlers. ROW ACTIONS are
 * deliberately NOT part of this -- each screen passes its own to `ListTable`,
 * which is why suppressing an action needs no flag here (see the tab's own
 * `mappingRowActions`). `shared/visionColumn.tsx` is the precedent for a
 * column factory living in `shared/`.
 *
 * Not to be merged with `RuntimeAdminSection`'s SPEC table columns: its first
 * two entries read the same two fields, but the other nine describe a spec.
 * It is a different table that shares two columns.
 */
export function mappingColumns(t: Translation): ListColumn<PortalModelMapping>[] {
  return [
    {
      id: 'gateway',
      label: t.mappingGatewayName,
      value: (m) => m.gateway_model_name,
      filter: 'text',
    },
    { id: 'app', label: t.mappingAppName, value: (m) => m.app_model_name, filter: 'text' },
    {
      id: 'status',
      label: t.tableStatus,
      value: (m) => m.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[applicationStatusLabelByKey[v as ApplicationStatus] ?? 'statusActive'],
      render: (m) => (
        <StatusChip status={m.status} label={t[applicationStatusLabelByKey[m.status]]} />
      ),
    },
    {
      id: 'context_size',
      label: t.mappingContextSize,
      value: (m) => formatMetric(m.context_size, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'energy_wh_per_token',
      label: t.mappingEnergyWhPerToken,
      // Watt-hours per single token: the significant digits live far behind the
      // decimal point, so this column needs a much longer tail than the others.
      value: (m) => formatMetric(m.energy_wh_per_token, 10),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'gen_tps',
      label: t.mappingGenTokensPerSecond,
      value: (m) => formatMetric(m.gen_tokens_per_second, 2),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'prompt_tps',
      label: t.mappingPromptTokensPerSecond,
      value: (m) => formatMetric(m.prompt_tokens_per_second, 2),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'load_time_ms',
      label: t.mappingLoadTimeMs,
      value: (m) => formatMetric(m.load_time_ms, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'is_mtp',
      label: t.mappingIsMtp,
      value: (m) => (m.is_mtp ? 'MTP' : '—'),
      filter: 'text',
      defaultHidden: true,
    },
    makeVisionColumn(t, (m) => m.vision_capable),
    {
      id: 'max_concurrency',
      label: t.mappingMaxConcurrency,
      value: (m) => formatMetric(m.max_concurrency, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'recommended_concurrency',
      label: t.mappingRecommendedConcurrency,
      value: (m) => formatMetric(m.recommended_concurrency, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'gen_tps_at_capacity',
      label: t.mappingGenTpsAtCapacity,
      value: (m) => formatMetric(m.gen_tokens_per_second_at_capacity, 2),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
  ];
}
