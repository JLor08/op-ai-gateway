// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { DashboardResponse } from '../api';
import type { Translation, MessageKey, RouteStatus } from './shared/types';
import { Box, TableRow, TableCell, Typography } from '@mui/material';
import { DataTable } from './shared/DataTable';
import { StatusChip } from './shared/StatusChip';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { StatTile } from './shared/StatTile';

type Metric = {
  labelKey: MessageKey;
  value: string;
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
          labelKey: 'tokens24h',
          value: String(dashboard.metrics.tokens_24h),
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
