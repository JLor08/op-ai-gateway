// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import {
  Box,
  Button,
  CircularProgress,
  Collapse,
  IconButton,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import KeyboardArrowDown from '@mui/icons-material/KeyboardArrowDown';
import KeyboardArrowRight from '@mui/icons-material/KeyboardArrowRight';
import { type ActivityQuery, type UsageEvent, type UsageGroupRow } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatCost, type CurrencyUnit } from '../currency';
import { formatEnergyWh } from './StatTiles';
import { SettingsMenu } from './SettingsMenu';
import { useColumnSettings } from './shared/useColumnSettings';
import { dimLabel } from './GroupByChainBuilder';

// Members shown per page inside an expanded group.
const MEMBER_LIMIT = 25;

// Configurable metric columns of the aggregate group table (the leading expand
// chevron + the group-label cell are structural and always shown). Mirrors the
// tile/table catalogues: a fixed id list + i18n label key + right/left align +
// default visibility; declaration order IS the default order. Cache-Write is the
// only column hidden by default — Cached is shown (per the feature request).
type GroupColId =
  | 'count'
  | 'error_count'
  | 'input_tokens'
  | 'output_tokens'
  | 'total_tokens'
  | 'cached_tokens'
  | 'cache_write_tokens'
  | 'energy_wh'
  | 'cost_eur'
  | 'span';

type GroupColDef = {
  id: GroupColId;
  labelKey: string;
  align: 'left' | 'right';
  defaultVisible: boolean;
};

const GROUP_COLUMNS: GroupColDef[] = [
  { id: 'count', labelKey: 'activityGroupCount', align: 'right', defaultVisible: true },
  { id: 'error_count', labelKey: 'activityErrorCount', align: 'right', defaultVisible: true },
  // Token block order (per request): Tokens, Prompt, Cached, Cache-Write (hidden), Generiert.
  { id: 'total_tokens', labelKey: 'tableTokens', align: 'right', defaultVisible: true },
  { id: 'input_tokens', labelKey: 'activityColInput', align: 'right', defaultVisible: true },
  { id: 'cached_tokens', labelKey: 'activityColCached', align: 'right', defaultVisible: true },
  {
    id: 'cache_write_tokens',
    labelKey: 'activityColCacheWrite',
    align: 'right',
    defaultVisible: false,
  },
  { id: 'output_tokens', labelKey: 'activityColOutput', align: 'right', defaultVisible: true },
  { id: 'energy_wh', labelKey: 'activityEnergyTile', align: 'right', defaultVisible: true },
  { id: 'cost_eur', labelKey: 'activityColCostEur', align: 'right', defaultVisible: true },
  { id: 'span', labelKey: 'activityGroupSpan', align: 'left', defaultVisible: true },
];

// Stable module-level defaults for useColumnSettings (an inline literal would
// churn identity every render). Mirrors DEFAULT_TILE_ORDER/DEFAULT_HIDDEN_TILES.
const DEFAULT_GROUP_ORDER: GroupColId[] = GROUP_COLUMNS.map((c) => c.id);
const DEFAULT_GROUP_HIDDEN: GroupColId[] = GROUP_COLUMNS.filter((c) => !c.defaultVisible).map(
  (c) => c.id,
);

// Maps a group-by dimension to the exact-filter param(s) the list endpoint needs
// to fetch that group's member requests. The list endpoint accepts the *_exact
// filters + the existing user_id/token_id/scope params; the extra keys ride the
// ActivityQuery open index signature (they aren't named fields).
function exactFilter(groupBy: string, key: string): ActivityQuery {
  // An empty key is the "(no value)" bucket (e.g. token-less rows, session-less
  // requests). buildQueryString drops params whose value is "", so the empty key
  // is encoded with a sentinel the backend maps back to an empty-value match:
  // token reuses the existing __none__ sentinel; session/server/model use __empty__.
  switch (groupBy) {
    case 'session':
      return { session_id_exact: key === '' ? '__empty__' : key };
    case 'server':
      return { server_exact: key === '' ? '__empty__' : key };
    case 'user':
      // Grouping by user is admin-only; the list must span all users for the pick.
      // KNOWN LIMIT: an empty user_id group can't be expanded exactly (rare/never in practice).
      return { user_id: key, scope: 'all' };
    case 'token':
      return { token_id: key === '' ? '__none__' : key };
    case 'model':
      return { model_exact: key === '' ? '__empty__' : key };
    case 'service':
      // No sentinel: the backend's service filter (HasServiceFilter) already
      // treats an empty value as "rows with no service attribution" — but
      // buildQueryString drops a "" param, so expanding the EMPTY-key group
      // (usage not attributed to any service) sends no filter at all and
      // shows every in-scope row instead. Mirrors the "user" dimension's
      // accepted known limit (an empty group can't be expanded exactly).
      return { service_id: key };
    case 'project':
      // Mirrors "session"/"server"/"model": the backend's ProjectIDExact takes
      // the __empty__ sentinel to mean "no project attribution" (spec:
      // 2026-08-08-projects-design.md §7 — UsageQuery.ProjectIDExact +
      // HasProjectIDExact, parseUsageQuery's __empty__ handling).
      return { project_id_exact: key === '' ? '__empty__' : key };
    default:
      return {};
  }
}

export type ActivityGroupsProps = {
  t: Translation;
  api: Pick<PortalApi, 'activity' | 'usageGroups'>;
  // The current ActivityQuery memo (scope + range + time + all column filters).
  // group_by is added by this component when fetching the aggregate rows.
  query: ActivityQuery;
  // Ordered dimension chain; chain[level] is grouped at this level. A level below
  // the last renders a nested ActivityGroups; the last expands to member requests.
  chain: string[];
  level?: number;
  showSettings?: boolean;
  costUnit: CurrencyUnit;
  currencyFactor: number;
  timeDisplay: 'absolute' | 'relative';
};

export function ActivityGroups({
  t,
  api,
  query,
  chain,
  level = 0,
  showSettings = true,
  costUnit,
  currencyFactor,
  timeDisplay,
}: Readonly<ActivityGroupsProps>) {
  const groupBy = chain[level] ?? '';
  const isLastLevel = level >= chain.length - 1;

  // Stabilize the query object's IDENTITY by deep-value (not just by reference).
  // An ancestor re-render (e.g. Activity.tsx's 1s relative-time ticker) recreates
  // a fresh `{ ...query, ...exactFilter(...) }` object for every open nested
  // child on every tick; without this, the fetch effects below (which depend on
  // `query` by identity) would refire every tick — flickering + collapsing any
  // deeper expansion. A deep-equal query from an ancestor re-render keeps the
  // SAME `stableQuery` identity, so the effects don't refire.
  const queryKey = JSON.stringify(query);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const stableQuery = useMemo(() => query, [queryKey]);

  const [rows, setRows] = useState<UsageGroupRow[] | null>(null);
  const [openKey, setOpenKey] = useState<string | null>(null);

  // Per-user column visibility + order for the aggregate table, persisted at the
  // profile exactly like the usage-table columns / stat tiles. Stored raw and
  // reconciled at read time (guards against localStorage corruption + stale ids).
  const {
    order: colOrder,
    hidden: hiddenCols,
    visibleIds: visibleColIds,
    toggle: toggleCol,
    reorder: reorderCol,
    reset: resetCols,
  } = useColumnSettings<GroupColId>('activity.groups', DEFAULT_GROUP_ORDER, DEFAULT_GROUP_HIDDEN);
  const visibleCols = useMemo(() => {
    const byId = new Map(GROUP_COLUMNS.map((c) => [c.id, c]));
    return visibleColIds.map((id) => byId.get(id) as GroupColDef);
  }, [visibleColIds]);

  // Member (expanded group) state; a single set since only one group is open.
  const [memberRows, setMemberRows] = useState<UsageEvent[]>([]);
  const [memberTotal, setMemberTotal] = useState(0);
  const [memberPages, setMemberPages] = useState(1);
  const [memberPage, setMemberPage] = useState(1);
  const [memberLoading, setMemberLoading] = useState(false);

  // Latest-wins guards (mirror Activity.tsx): only the newest fetch commits, so a
  // slow response can't clobber a newer one.
  const reqIdRef = useRef(0);
  const memberReqIdRef = useRef(0);

  function timeCell(iso: string): string {
    if (!iso) return '-';
    if (timeDisplay === 'relative')
      return t.activityRelativeTime((Date.now() - new Date(iso).getTime()) / 1000);
    return new Date(iso).toLocaleString();
  }

  function groupLabel(row: UsageGroupRow): string {
    if (groupBy === 'token' && row.key === '') return t.activityGroupTokenNone;
    // An empty service key means the usage was NOT attributed to any Service
    // Account (ordinary user-token/session usage) — the backend echoes an empty
    // key_label for this case (portal.usageGroupLabel), so label it explicitly.
    if (groupBy === 'service' && row.key === '') return t.activityGroupServiceNone;
    // Mirrors the service case: an empty project key means the usage was NOT
    // attributed to any project (backend's usageGroupLabel returns "" as-is
    // for an empty key, same as service/token).
    if (groupBy === 'project' && row.key === '') return t.activityGroupProjectNone;
    return row.key_label || row.key;
  }

  // Rendered content of a configurable metric cell for a group row.
  function cellValue(id: GroupColId, row: UsageGroupRow): string | number {
    switch (id) {
      case 'count':
        return row.count;
      case 'error_count':
        return row.error_count;
      case 'input_tokens':
        return row.input_tokens;
      case 'output_tokens':
        return row.output_tokens;
      case 'total_tokens':
        return row.total_tokens;
      case 'cached_tokens':
        return row.cached_tokens;
      case 'cache_write_tokens':
        return row.cache_write_tokens;
      case 'energy_wh':
        return formatEnergyWh(row.energy_wh);
      case 'cost_eur':
        return formatCost(row.cost_eur, costUnit, currencyFactor);
      case 'span':
        return `${timeCell(row.first_at)} – ${timeCell(row.last_at)}`;
    }
  }

  // Column-settings menu, shown above the table in every state (loading/empty/data)
  // so the picker is always reachable. Reuses the generic SettingsMenu (the same
  // checkbox + drag-reorder + reset control the tiles use).
  const settingsBar = (
    <Box sx={{ display: 'flex', justifyContent: 'flex-end', mb: 1 }}>
      <SettingsMenu
        items={GROUP_COLUMNS.map((c) => ({
          id: c.id,
          label: t[c.labelKey as keyof typeof t] as string,
        }))}
        hidden={hiddenCols}
        order={colOrder}
        onToggle={toggleCol}
        onReorder={reorderCol}
        onReset={resetCols}
        buttonLabel={t.listColumns}
        title={t.listColumns}
        resetLabel={t.listColumnsReset}
        moveLeftLabel={t.listColumnMoveLeft}
        moveRightLabel={t.listColumnMoveRight}
      />
    </Box>
  );

  // Fetch the aggregate rows on query/groupBy change. Best-effort: an error just
  // clears the list (empty state), never throws to the page.
  useEffect(() => {
    const myId = ++reqIdRef.current;
    setRows(null);
    setOpenKey(null);
    void (async () => {
      try {
        const resp = await api.usageGroups({ ...stableQuery, group_by: groupBy });
        if (reqIdRef.current !== myId) return;
        setRows(resp.data);
      } catch {
        if (reqIdRef.current !== myId) return;
        setRows([]);
      }
    })();
  }, [api, stableQuery, groupBy]);

  // Fetch the open group's member requests for the current inner page. Only on
  // the last level of the chain — an intermediate level renders a nested
  // ActivityGroups instead (see the Collapse branch below).
  useEffect(() => {
    if (openKey === null || !isLastLevel) return;
    const myId = ++memberReqIdRef.current;
    setMemberLoading(true);
    void (async () => {
      try {
        const resp = await api.activity({
          ...stableQuery,
          ...exactFilter(groupBy, openKey),
          page: memberPage,
          limit: MEMBER_LIMIT,
        });
        if (memberReqIdRef.current !== myId) return;
        setMemberRows(resp.data);
        setMemberTotal(resp.total);
        setMemberPages(resp.total_pages);
      } catch {
        if (memberReqIdRef.current !== myId) return;
        setMemberRows([]);
        setMemberTotal(0);
        setMemberPages(1);
      } finally {
        if (memberReqIdRef.current === myId) setMemberLoading(false);
      }
    })();
  }, [api, stableQuery, groupBy, openKey, memberPage, isLastLevel]);

  const toggle = useCallback((key: string) => {
    setOpenKey((cur) => (cur === key ? null : key));
    // Reset the inner pagination whenever a group is toggled (open or switch).
    setMemberRows([]);
    setMemberTotal(0);
    setMemberPages(1);
    setMemberPage(1);
  }, []);

  if (rows === null) {
    return (
      <>
        {showSettings && settingsBar}
        <Box sx={{ display: 'flex', justifyContent: 'center', py: 4 }}>
          <CircularProgress size={28} />
        </Box>
      </>
    );
  }

  if (rows.length === 0) {
    return (
      <>
        {showSettings && settingsBar}
        <Typography variant="body2" color="text.secondary" sx={{ py: 3, textAlign: 'center' }}>
          {t.activityEmpty}
        </Typography>
      </>
    );
  }

  // The last-level expanded row's member-request detail: a loading spinner,
  // an empty hint, or the member table + its own pager. Only ever rendered
  // for the single currently-open row (isLastLevel + openKey gate the caller).
  function memberDetailContent(): ReactNode {
    if (memberLoading && memberRows.length === 0) {
      return (
        <Box sx={{ display: 'flex', justifyContent: 'center', py: 2 }}>
          <CircularProgress size={20} />
        </Box>
      );
    }
    if (memberRows.length === 0) {
      return (
        <Typography variant="body2" color="text.secondary" sx={{ py: 1 }}>
          {t.activityEmpty}
        </Typography>
      );
    }
    return (
      <>
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>{t.tableModel}</TableCell>
              <TableCell>{t.tableHost}</TableCell>
              <TableCell align="right">{t.tableTokens}</TableCell>
              <TableCell align="right">{t.tableStatus}</TableCell>
              <TableCell>{t.activityColTime}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {memberRows.map((m) => (
              <TableRow key={m.id}>
                <TableCell>{m.model}</TableCell>
                <TableCell>{m.server_name || '-'}</TableCell>
                <TableCell align="right">{m.total_tokens}</TableCell>
                <TableCell align="right">
                  {m.http_status || (m.status === 'error' ? t.no : t.yes)}
                </TableCell>
                <TableCell>{timeCell(m.created_at)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        {memberPages > 1 && (
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'flex-end',
              gap: 1,
              mt: 1,
            }}
          >
            <Typography variant="caption" color="text.secondary">
              {memberPage} {t.listRangeOf} {memberPages} · {memberTotal} {t.listRowsSuffix}
            </Typography>
            <Button
              size="small"
              disabled={memberPage <= 1 || memberLoading}
              onClick={() => setMemberPage((p) => Math.max(1, p - 1))}
            >
              {t.activityPrevPage}
            </Button>
            <Button
              size="small"
              disabled={memberPage >= memberPages || memberLoading}
              onClick={() => setMemberPage((p) => p + 1)}
            >
              {t.activityNextPage}
            </Button>
          </Box>
        )}
      </>
    );
  }

  return (
    <>
      {showSettings && settingsBar}
      <TableContainer>
        <Table size="small" sx={{ minWidth: 720 }}>
          <TableHead>
            <TableRow>
              <TableCell sx={{ width: 40 }} />
              <TableCell>{dimLabel(t, groupBy)}</TableCell>
              {visibleCols.map((c) => (
                <TableCell key={c.id} align={c.align === 'right' ? 'right' : 'left'}>
                  {t[c.labelKey as keyof typeof t] as string}
                </TableCell>
              ))}
            </TableRow>
          </TableHead>
          <TableBody>
            {rows.map((row) => {
              const open = openKey === row.key;
              return (
                <Fragment key={row.key}>
                  <TableRow hover sx={{ cursor: 'pointer' }} onClick={() => toggle(row.key)}>
                    <TableCell sx={{ width: 40 }}>
                      <IconButton
                        size="small"
                        aria-label={groupLabel(row)}
                        onClick={(e) => {
                          e.stopPropagation();
                          toggle(row.key);
                        }}
                      >
                        {open ? (
                          <KeyboardArrowDown fontSize="small" />
                        ) : (
                          <KeyboardArrowRight fontSize="small" />
                        )}
                      </IconButton>
                    </TableCell>
                    <TableCell>{groupLabel(row)}</TableCell>
                    {visibleCols.map((c) => (
                      <TableCell key={c.id} align={c.align === 'right' ? 'right' : 'left'}>
                        {cellValue(c.id, row)}
                      </TableCell>
                    ))}
                  </TableRow>
                  <TableRow>
                    <TableCell
                      colSpan={visibleCols.length + 2}
                      sx={{ py: 0, borderBottom: open ? undefined : 'none' }}
                    >
                      <Collapse in={open} timeout="auto" unmountOnExit>
                        <Box sx={{ my: 1 }}>
                          {isLastLevel ? (
                            memberDetailContent()
                          ) : (
                            <ActivityGroups
                              t={t}
                              api={api}
                              query={{ ...stableQuery, ...exactFilter(groupBy, row.key) }}
                              chain={chain}
                              level={level + 1}
                              showSettings={false}
                              costUnit={costUnit}
                              currencyFactor={currencyFactor}
                              timeDisplay={timeDisplay}
                            />
                          )}
                        </Box>
                      </Collapse>
                    </TableCell>
                  </TableRow>
                </Fragment>
              );
            })}
          </TableBody>
        </Table>
      </TableContainer>
    </>
  );
}
