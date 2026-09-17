// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type ReactNode } from 'react';
import type { DashboardResponse } from '../api';
import type { Translation, MessageKey, RouteStatus } from './shared/types';
import { Box, TableRow, TableCell, Typography } from '@mui/material';
import { DataTable } from './shared/DataTable';
import { StatusChip } from './shared/StatusChip';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { StatTile } from './shared/StatTile';
import { TokenAggregateValue } from './TokenAggregateValue';

type Metric = {
  labelKey: MessageKey;
  // ReactNode, not string: StatTile has always rendered `value` as a ReactNode,
  // and this local narrowing was the only thing standing between the 24h token
  // tile and the same marker-plus-tooltip the tables get.
  value: ReactNode;
  detailKey: MessageKey;
};

const routeStatusLabelByKey: Record<RouteStatus, MessageKey> = {
  active: 'statusActive',
  watch: 'statusWatch',
  standby: 'statusStandby',
};

export function Dashboard({
  t,
  dashboard,
  productName,
}: Readonly<{
  t: Translation;
  dashboard: DashboardResponse | null;
  productName: string;
}>) {
  const metricRows: Metric[] = dashboard
    ? [
        {
          labelKey: 'requests24h',
          value: String(dashboard.metrics.requests_24h),
          detailKey: 'requests24hDetail',
        },
        {
          // The same three-state rule the Activity tiles use (#70), through the
          // same component: a 24h window whose requests are ALL non-token-metered
          // has no token figure at all, and a MIXED one shows the sum of its
          // token-metered subset with the marker and the tooltip naming what the
          // sum leaves out. Rendering a bare number in either case would assert a
          // coverage the figure does not have.
          labelKey: 'tokens24h',
          value: (
            <TokenAggregateValue
              value={dashboard.metrics.tokens_24h}
              nonTokenRequests={dashboard.metrics.non_token_requests_24h ?? 0}
              totalRequests={dashboard.metrics.requests_24h}
              t={t}
            />
          ),
          detailKey: 'tokens24hDetail',
        },
        {
          labelKey: 'healthyHosts',
          value: dashboard.metrics.healthy_hosts,
          detailKey: 'healthyHostsDetail',
        },
        {
          labelKey: 'latencyP95',
          value: `${dashboard.metrics.latency_p95_ms} ms`,
          detailKey: 'latencyP95Detail',
        },
      ]
    : [];
  const routes = dashboard?.routes ?? [];

  return (
    <>
      <PageTitle title={t.dashboard} subtitle={`${t.welcomeTo} ${productName}`} />

      <Box
        component="section"
        aria-label={t.gatewayMetrics}
        sx={{
          display: 'grid',
          gridTemplateColumns: {
            xs: '1fr',
            sm: 'repeat(2, minmax(150px, 1fr))',
            md: 'repeat(4, minmax(150px, 1fr))',
          },
          gap: 2,
          mb: 3.5,
        }}
      >
        {metricRows.length === 0 && (
          <Typography sx={{ m: 0, fontWeight: 700 }}>{t.loading}</Typography>
        )}
        {metricRows.map((metric) => (
          <StatTile
            key={metric.labelKey}
            value={metric.value}
            label={t[metric.labelKey]}
            detail={t[metric.detailKey]}
          />
        ))}
      </Box>

      <Panel titleId="route-heading" title={t.liveModelRoutes} subtitle={t.liveModelRoutesSubtitle}>
        <DataTable
          columns={[t.tableModel, t.tableProvider, t.tableHost, t.tableStatus]}
          isEmpty={routes.length === 0}
          emptyLabel={dashboard ? t.modelsEmpty : t.loading}
        >
          {routes.map((row) => (
            <TableRow key={`${row.model}-${row.host}`}>
              <TableCell>{row.model}</TableCell>
              <TableCell>{row.provider}</TableCell>
              <TableCell>{row.host}</TableCell>
              <TableCell>
                <StatusChip status={row.status} label={t[routeStatusLabelByKey[row.status]]} />
              </TableCell>
            </TableRow>
          ))}
        </DataTable>
      </Panel>
    </>
  );
}
