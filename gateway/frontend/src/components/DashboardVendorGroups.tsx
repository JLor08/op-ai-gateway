// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Paper, Typography } from '@mui/material';
import type { VendorAccountGroup, VendorAccountGroupMember } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { usePolledFetch } from './shared/usePolledFetch';
import { reasonLabel } from './shared/vendorGroupLabels';

// Same cadence as DashboardVendorUsage: the group list is a database read of what
// the gateway already knows (it never reaches a vendor), so a minute is gentle.
const GROUPS_POLL_MS = 60_000;

const localTime = (iso: string) =>
  new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

// Which window the member's `pct` belongs to: the one of its usage snapshot with the
// same figure. Empty when the snapshot is absent or matches none (nothing is guessed).
function bindingWindow(t: Translation, member: VendorAccountGroupMember): string {
  const usage = member.usage;
  if (!usage || member.pct === undefined) return '';
  if (usage.five_hour_pct === member.pct) return t.dashboardVendorGroupWindowFiveHour;
  if (usage.weekly_pct === member.pct) return t.dashboardVendorGroupWindowWeekly;
  if (usage.spend_used_pct === member.pct) return t.dashboardVendorGroupWindowCredits;
  return '';
}

function memberLine(t: Translation, member: VendorAccountGroupMember): string {
  switch (member.state) {
    case 'active':
      return t.dashboardVendorGroupLineActive;
    case 'available':
      return t.dashboardVendorGroupLineReady;
    case 'over_threshold': {
      if (member.pct === undefined) return t.dashboardVendorGroupLineOverThresholdBare;
      return t.dashboardVendorGroupLineOverThreshold(
        `${bindingWindow(t, member)} ${member.pct} %`.trim(),
        member.free_at ? localTime(member.free_at) : '',
      );
    }
    case 'cooldown':
      return (member.cooldowns ?? [])
        .map((c) => t.dashboardVendorGroupLineCooldown(localTime(c.until), c.model))
        .join('; ');
    default:
      return t.dashboardVendorGroupLineNotUsable(reasonLabel(t, member.reason));
  }
}

function memberName(t: Translation, member: VendorAccountGroupMember): string {
  return member.owner_name
    ? `${member.account_name} (${t.vendorAccountSharedBy} ${member.owner_name})`
    : member.account_name;
}

function GroupCard({ t, group }: Readonly<{ t: Translation; group: VendorAccountGroup }>) {
  const headingId = `dashboard-vendor-group-${group.id}`;
  const active = group.members.find((m) => m.account_id === group.active_member_id);
  return (
    <Paper
      component="li"
      variant="outlined"
      aria-labelledby={headingId}
      sx={{ p: 2, display: 'grid', gap: 1, alignContent: 'start', listStyle: 'none' }}
    >
      <Box>
        <Typography
          component="h3"
          id={headingId}
          variant="subtitle1"
          sx={{ fontWeight: 600, overflowWrap: 'anywhere' }}
        >
          {group.name}
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
          {active
            ? t.dashboardVendorGroupActiveMember(memberName(t, active))
            : t.dashboardVendorGroupNoActiveMember}
        </Typography>
      </Box>
      <Box component="ul" sx={{ m: 0, p: 0, listStyle: 'none', display: 'grid', gap: 0.5 }}>
        {group.members.map((member) => (
          <Box component="li" key={member.account_id}>
            <Typography variant="body2" component="span" sx={{ overflowWrap: 'anywhere' }}>
              {memberName(t, member)}
            </Typography>{' '}
            <Typography variant="body2" component="span" color="text.secondary">
              {memberLine(t, member)}
            </Typography>
          </Box>
        ))}
      </Box>
    </Paper>
  );
}

/**
 * The dashboard's "Anbieter -- Gruppen" section: one card per active vendor account
 * group with its active member and one status line per member. It renders nothing
 * without an active group, before the first read, and when the read fails.
 *
 * Mount it only when the vendor-accounts feature is on (the parent decides): it
 * fetches as soon as it mounts.
 */
export function DashboardVendorGroups({
  t,
  api,
}: Readonly<{ t: Translation; api: Pick<PortalApi, 'vendorAccountGroups'> }>) {
  const { data } = usePolledFetch(() => api.vendorAccountGroups(), [api], {
    intervalMs: GROUPS_POLL_MS,
    live: true,
  });
  const groups = (data?.groups ?? []).filter((group) => group.status === 'active');
  if (groups.length === 0) return null;
  return (
    <Box sx={{ mb: 3.5 }}>
      <Panel titleId="dashboard-vendor-groups-heading" title={t.dashboardVendorGroupsTitle}>
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
          {groups.map((group) => (
            <GroupCard key={group.id} t={t} group={group} />
          ))}
        </Box>
      </Panel>
    </Box>
  );
}
