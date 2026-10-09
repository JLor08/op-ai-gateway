// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Paper, Typography } from '@mui/material';
import type { VendorAccount } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { StatusChip } from './shared/StatusChip';
import { usePolledFetch } from './shared/usePolledFetch';
import { vendorLabel } from './shared/vendorLabel';
import { vendorStatusBadge, vendorStatusLabel } from './shared/vendorStatus';
import { VendorAccountUsageRows, visibleUsageRows } from './VendorAccountUsageRows';

// How often the stored snapshots are re-read while the dashboard is open. The list
// read is a database read of what the gateway already learned from the requests it
// served; it never reaches the vendor, so a minute is gentle and keeps the figures
// and the countdowns from going stale on a dashboard left open.
const USAGE_POLL_MS = 60_000;

/**
 * Whether an account can have limits worth listing: an api-key account, or a
 * subscription that is connected. A subscription that was never connected (or whose
 * tokens were removed) has nothing to report, whatever stale snapshot it may carry.
 */
function canReportUsage(account: VendorAccount): boolean {
  return account.auth_type === 'api_key' || account.subscription_connected;
}

function UsageCard({ t, account }: Readonly<{ t: Translation; account: VendorAccount }>) {
  const headingId = `dashboard-vendor-usage-${account.id}`;
  return (
    <Paper
      component="li"
      variant="outlined"
      aria-labelledby={headingId}
      sx={{ p: 2, display: 'grid', gap: 1.5, alignContent: 'start', listStyle: 'none' }}
    >
      <Box>
        <Box
          sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 1 }}
        >
          <Typography
            component="h3"
            id={headingId}
            variant="subtitle1"
            sx={{ fontWeight: 600, overflowWrap: 'anywhere' }}
          >
            {account.name}
          </Typography>
          {account.status !== 'active' && (
            <StatusChip
              status={vendorStatusBadge(account.status)}
              label={vendorStatusLabel(t, account.status)}
            />
          )}
        </Box>
        <Typography variant="body2" color="text.secondary">
          {vendorLabel(t, account.vendor)}
        </Typography>
      </Box>
      {account.usage && <VendorAccountUsageRows t={t} usage={account.usage} dense />}
    </Paper>
  );
}

/**
 * The dashboard's "Anbieter -- Nutzung & Limits" section: one compact card per
 * vendor account that has limits to show (VendorAccountUsageRows, dense), so the
 * user sees at a glance how full each of their accounts is without opening it.
 *
 * It reads the vendor-accounts LIST, whose rows carry the usage snapshot the
 * gateway has stored; it never asks a vendor (no refresh call), so it is cheap and
 * safe to poll. An account appears only when it can report usage (canReportUsage)
 * and has at least one visible row (visibleUsageRows): a Business account shows
 * just its credit row, and one with no snapshot yet is left out rather than drawn
 * as an empty card. With no such account -- or before the first read, or when the
 * read fails -- the section renders nothing at all: the dashboard keeps its
 * metrics and routes, and the per-account detail view carries the empty state.
 *
 * Mount it only when the vendor-accounts feature is on: it fetches as soon as it
 * mounts, and the parent (Dashboard) is what keeps a disabled feature from ever
 * making the call.
 */
export function DashboardVendorUsage({
  t,
  api,
}: Readonly<{ t: Translation; api: Pick<PortalApi, 'vendorAccounts'> }>) {
  const { data } = usePolledFetch(() => api.vendorAccounts(), [api], {
    intervalMs: USAGE_POLL_MS,
    live: true,
  });
  const accounts = (data?.data ?? []).filter(
    (account) => canReportUsage(account) && visibleUsageRows(account.usage).length > 0,
  );
  if (accounts.length === 0) return null;
  return (
    <Box sx={{ mb: 3.5 }}>
      <Panel titleId="dashboard-vendor-usage-heading" title={t.dashboardVendorUsageTitle}>
        <Box
          component="ul"
          sx={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))',
            gap: 2,
            m: 0,
            p: 0,
            listStyle: 'none',
          }}
        >
          {accounts.map((account) => (
            <UsageCard key={account.id} t={t} account={account} />
          ))}
        </Box>
      </Panel>
    </Box>
  );
}
