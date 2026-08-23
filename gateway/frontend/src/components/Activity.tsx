// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useMemo, useRef, useState, type MouseEvent } from 'react';
import { Alert, Box, Button } from '@mui/material';
import {
  PortalApiError,
  type AdminUser,
  type ActivityQuery,
  type CaptureDetail,
  type PortalToken,
  type UsageEvent,
} from '../api';
import type { PortalApi, Translation } from './shared/types';
import { availableUnits, type CurrencyUnit } from '../currency';
import { formatPortalError } from './shared/format';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { SearchableSelect, type SearchableOption } from './shared/SearchableSelect';
import { StatTiles } from './StatTiles';
import { SettingsMenu } from './SettingsMenu';
import {
  ACTIVITY_TILES,
  DEFAULT_TILE_ORDER,
  DEFAULT_HIDDEN_TILES,
  type TileId,
} from './activityTiles';
import { ActivityTable } from './ActivityTable';
import { ActivityGroups } from './ActivityGroups';
import { ActiveRequestsPanel } from './ActiveRequestsPanel';
import { ActivityCharts } from './ActivityCharts';
import { useActivityData } from './useActivityData';
import { CaptureDialog } from './CaptureDialog';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { useToast } from './shared/ToastProvider';
import {
  ActivityToolbar,
  type ActivityRange,
  type ActivityScope,
  type ActivityStatusFilter,
} from './ActivityToolbar';
import { ColumnMenu } from './ColumnMenu';
import { usePreference } from './shared/preferences';
import { useColumnSettings } from './shared/useColumnSettings';
import {
  ACTIVITY_COLUMNS,
  DEFAULT_HIDDEN_COLUMNS,
  DEFAULT_COLUMN_ORDER,
  readScope,
  writeScope,
  readFilterUser,
  writeFilterUser,
  readFilterToken,
  writeFilterToken,
  readTsWindow,
  writeTsWindow,
  readTsBucket,
  writeTsBucket,
  type ColumnId,
  type TsWindow,
  type TsBucket,
} from './activityColumns';

// Text filters keyed by ActivityTable column id -> the backend query param name.
// (server_name -> server; the rest match.) "owner" filters the owner column.
const TEXT_FILTER_PARAM: Record<string, string> = {
  model: 'model',
  server_name: 'server',
  req_path: 'req_path',
  content_type: 'content_type',
  provider_path: 'provider_path',
  provider_model: 'provider_model',
  session: 'session_id',
  agent_id: 'agent_id',
  // service_name is the DISPLAY column; the backend's service filter is exact
  // (mirrors token_id, per usage.UsageQuery.ServiceID/HasServiceFilter) so the
  // typed value is sent verbatim as service_id.
  service_name: 'service_id',
  owner: 'owner',
};

// Wire value for the chat/no-token option: the backend maps it to token_id == "".
const NO_TOKEN_SENTINEL = '__none__';

// STABLE default reference for the custom-range preference. usePreference returns
// the default BY REFERENCE when no value is stored, so an inline object literal
// would change identity every render and churn the `query` memo (whose dep list
// includes customRange) — refiring the load effect on every render. A module-level
// constant keeps the identity stable, exactly like DEFAULT_HIDDEN_COLUMNS/ORDER.
const EMPTY_CUSTOM_RANGE = { from: '', to: '' };

// STABLE default reference for the group-by chain preference (mirrors EMPTY_CUSTOM_RANGE
// above — usePreference returns the default BY REFERENCE, so an inline [] would churn
// identity every render).
const EMPTY_CHAIN: string[] = [];

// Option label for a currency-unit dropdown entry.
function unitLabel(t: Translation, u: CurrencyUnit): string {
  switch (u) {
    case 'eur':
      return t.currencyUnitEur;
    case 'eur_cent':
      return t.currencyUnitEurCent;
    case 'usd':
      return t.currencyUnitUsd;
    case 'usd_cent':
      return t.currencyUnitUsdCent;
  }
}

// Pure derivation of the effective scope + user/token wire filters from the
// caller's role and the current scope/user/token selections. Cross-user views
// are admin-gated (role admin OR system_admin); non-admins are forced to their
// own activity (defence in depth; the scope switch is admin-only). The token
// dropdown shows for the own scope (incl. every non-admin) or for a specific
// user once one is chosen; never for the all-users scope. Wire values: the
// "user" scope rides the admin all-scope path and is pinned to the chosen user
// via user_id (which the backend honors regardless of the scope flag); an
// empty user selection collapses "user" back to no filter.
function deriveScopeFilters(
  role: string,
  scope: ActivityScope,
  selectedUser: string,
  selectedToken: string,
) {
  const isAdmin = role === 'admin' || role === 'system_admin';
  const effectiveScope: ActivityScope = isAdmin ? scope : 'own';
  const showTokenFilter =
    effectiveScope === 'own' || (effectiveScope === 'user' && selectedUser !== '');
  const scopeParam: 'own' | 'all' = effectiveScope === 'user' ? 'all' : effectiveScope;
  const filterUserId = effectiveScope === 'user' && selectedUser !== '' ? selectedUser : undefined;
  let filterTokenId: string | undefined;
  if (showTokenFilter && selectedToken !== '') {
    filterTokenId = selectedToken === 'chat-session' ? NO_TOKEN_SENTINEL : selectedToken;
  }
  return { isAdmin, effectiveScope, showTokenFilter, scopeParam, filterUserId, filterTokenId };
}

export type ActivityProps = {
  t: Translation;
  api: Pick<
    PortalApi,
    | 'activeRequests'
    | 'activity'
    | 'activityStats'
    | 'adminUsers'
    | 'captureDetail'
    | 'deleteCapture'
    | 'getCurrency'
    | 'setCaptureSecret'
    | 'subscribeActivity'
    | 'tokens'
    | 'usageGroups'
    | 'usageTimeSeries'
    | 'userTokens'
  >;
  role: string;
  onUnauthorized: () => void;
};

export function Activity({ t, api, role, onUnauthorized }: Readonly<ActivityProps>) {
  const [q, setQ] = useState('');
  // Substring text filters keyed by column id (model/server_name/req_path/
  // content_type/provider_model/owner) -> mapped to backend params in `query`.
  const [textFilters, setTextFilters] = useState<Record<string, string>>({});
  const [status, setStatus] = useState<ActivityStatusFilter>('');
  const [stream, setStream] = useState<'' | 'true' | 'false'>('');
  // Per-column numeric range filters keyed by column id; plus a created_at range.
  const [numeric, setNumeric] = useState<Record<string, { min: string; max: string }>>({});
  const [timeFrom, setTimeFrom] = useState('');
  const [timeTo, setTimeTo] = useState('');
  // Zeit-column display mode (absolute date&time vs. relative "vor X"), persisted
  // at the user profile (see usePreference below for order/hidden/ownerDisplay).
  const [timeDisplay, setTimeDisplay] = usePreference<'absolute' | 'relative'>(
    'table.activity.timeDisplay',
    'absolute',
  );
  // Cost-display unit for the Usage-list cost column + the aggregate cost tile,
  // persisted at the user profile exactly like timeDisplay above. The USD-per-EUR
  // conversion factor is fetched once on mount (best-effort; <= 0 means USD is
  // unavailable, per availableUnits()).
  const [costUnit, setCostUnit] = usePreference<CurrencyUnit>('activity.costUnit', 'eur_cent');
  const [currencyFactor, setCurrencyFactor] = useState(0);
  // Collapsed state of the whole time-series charts section (header + the
  // window/resolution dropdowns + the line charts). Purely presentational,
  // persisted at the profile; the timeseries fetch keeps running regardless.
  const [chartsCollapsed, setChartsCollapsed] = usePreference<boolean>(
    'activity.chartsCollapsed',
    false,
  );
  // Ordered group-by dimension chain ([] = off = the flat usage table). Persisted
  // at the profile like the other Activity view preferences. Available to every user.
  const [groupChain, setGroupChain] = usePreference<string[]>('activity.groupByChain', EMPTY_CHAIN);
  // One-time seed: a grouping set via the earlier single-dropdown (activity.groupBy)
  // is honored until the user edits the chain. Reading it keeps that value alive.
  // The seed is gated on the chain preference being UNSET (still the identity-stable
  // EMPTY_CHAIN default), not merely empty: usePreference returns the default BY
  // REFERENCE only when no value is stored, so a stored [] (the user explicitly
  // turned grouping off) is a distinct array and must win over the legacy seed —
  // else removing the last chip would resurrect grouping and the flat table would
  // become unreachable for anyone with a legacy activity.groupBy value.
  const [legacyGroupBy] = usePreference<string>('activity.groupBy', '');
  let effectiveChain: string[];
  if (groupChain !== EMPTY_CHAIN) {
    effectiveChain = groupChain;
  } else {
    effectiveChain = legacyGroupBy ? [legacyGroupBy] : [];
  }
  const [range, setRange] = useState<ActivityRange>('30d');
  // Custom absolute time range (datetime-local strings), used only when range ===
  // "custom". Persisted at the profile like the other Activity view preferences.
  // In custom mode the query sends these as time_from/time_to (ungated) + range=all
  // so the preset lower bound doesn't also clip; presets keep the existing per-column
  // created_at gating.
  const [customRange, setCustomRange] = usePreference<{ from: string; to: string }>(
    'activity.customRange',
    EMPTY_CUSTOM_RANGE,
  );
  const [scope, setScope] = useState<ActivityScope>(() => readScope());
  const [selectedUser, setSelectedUser] = useState<string>(() => readFilterUser());
  const [selectedToken, setSelectedToken] = useState<string>(() => readFilterToken());
  const [userOptions, setUserOptions] = useState<AdminUser[]>([]);
  const [tokenOptions, setTokenOptions] = useState<PortalToken[]>([]);
  const [sort, setSort] = useState('created_at');
  const [order, setOrder] = useState<'asc' | 'desc'>('desc');
  const [page, setPage] = useState(1);
  const [limit, setLimit] = useState(25);

  const [tsWindow, setTsWindow] = useState<TsWindow>(() => readTsWindow());
  const [tsBucket, setTsBucket] = useState<TsBucket>(() => readTsBucket());

  // Table column settings persisted at the user profile. hidden/order are stored
  // raw and reconciled against the current columns at read time.
  const {
    order: columnOrder,
    hidden,
    toggle: toggleColumn,
    reorder: reorderColumn,
    reset: resetColumns,
  } = useColumnSettings<ColumnId>('table.activity', DEFAULT_COLUMN_ORDER, DEFAULT_HIDDEN_COLUMNS);
  const [ownerDisplay, setOwnerDisplay] = usePreference<'name' | 'id'>(
    'table.activity.ownerDisplay',
    'name',
  );
  // Stat-tile settings persisted at the user profile, mirroring the table columns
  // above. hidden/order stored raw; order reconciled against the tile catalogue.
  const {
    order: tileOrder,
    hidden: tilesHidden,
    toggle: toggleTile,
    reorder: reorderTile,
    reset: resetTiles,
  } = useColumnSettings<TileId>('activity.tiles', DEFAULT_TILE_ORDER, DEFAULT_HIDDEN_TILES);
  const [columnsAnchor, setColumnsAnchor] = useState<HTMLElement | null>(null);
  const [detailRow, setDetailRow] = useState<UsageEvent | null>(null);
  const [detail, setDetail] = useState<CaptureDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState('');
  const [confirmingDeleteCapture, setConfirmingDeleteCapture] = useState(false);

  const { isAdmin, effectiveScope, showTokenFilter, scopeParam, filterUserId, filterTokenId } =
    deriveScopeFilters(role, scope, selectedUser, selectedToken);

  // Column ids currently VISIBLE. A per-column filter is only sent to the backend
  // while its column is visible — otherwise a filter on a hidden/scope-gated column
  // (whose clear affordance, the header filter icon, is gone) would silently narrow
  // results with no way to clear it (e.g. an owner filter surviving a switch to
  // own-scope). Reconcile inline since visibleColumns is computed further down.
  const visibleColumnIds = useMemo(() => {
    const showOwner = effectiveScope === 'all';
    const set = new Set<string>();
    for (const id of columnOrder) {
      if (id === 'owner' && !showOwner) continue;
      if (hidden.includes(id)) continue;
      set.add(id);
    }
    return set;
  }, [columnOrder, hidden, effectiveScope]);

  const query = useMemo<ActivityQuery>(() => {
    const numericParams: Record<string, number> = {};
    for (const [col, r] of Object.entries(numeric)) {
      if (!visibleColumnIds.has(col)) continue;
      if (r.min.trim() !== '') numericParams[`${col}_min`] = Number(r.min);
      if (r.max.trim() !== '') numericParams[`${col}_max`] = Number(r.max);
    }
    const textParams: Record<string, string> = {};
    for (const [col, value] of Object.entries(textFilters)) {
      const param = TEXT_FILTER_PARAM[col];
      if (param && value.trim() !== '' && visibleColumnIds.has(col)) textParams[param] = value;
    }
    return {
      page,
      limit,
      sort,
      order,
      status: visibleColumnIds.has('http_status') ? status : '',
      q,
      scope: scopeParam,
      user_id: filterUserId,
      token_id: filterTokenId,
      stream: visibleColumnIds.has('stream') ? stream : '',
      // Custom mode replaces the preset lower bound with an absolute from/to window
      // (range=all so the preset doesn't also clip) and sends the bounds UNGATED.
      // Every preset keeps the existing behavior byte-identical: the range preset
      // plus the per-column created_at datetime filter (only while that column is
      // visible, ANDed on top).
      ...(range === 'custom'
        ? {
            range: 'all' as const,
            time_from: customRange.from || undefined,
            time_to: customRange.to || undefined,
          }
        : {
            range,
            time_from: visibleColumnIds.has('created_at') ? timeFrom : '',
            time_to: visibleColumnIds.has('created_at') ? timeTo : '',
          }),
      ...textParams,
      ...numericParams,
    };
  }, [
    page,
    limit,
    sort,
    order,
    status,
    q,
    range,
    customRange,
    scopeParam,
    filterUserId,
    filterTokenId,
    stream,
    timeFrom,
    timeTo,
    textFilters,
    numeric,
    visibleColumnIds,
  ]);

  const onUnauthorizedRef = useRef(onUnauthorized);
  onUnauthorizedRef.current = onUnauthorized;
  const tRef = useRef(t);
  tRef.current = t;
  const { showError } = useToast();
  const detailReqIdRef = useRef(0);

  // Whether the current view is page 1, sorted by created_at desc -- only that
  // view's list refetches on an SSE signal (see useActivityData below).
  const newest = page === 1 && sort === 'created_at' && order === 'desc';

  // Data-orchestration layer: the list/stats/active/time-series fetches, their
  // monotonic request-id guards, the SSE-driven refetch (with its asymmetric
  // guard bump-and-release), and the leading-edge SSE throttle. Extracted
  // verbatim into useActivityData; the container only supplies inputs and
  // renders the result.
  const { pageData, stats, timeSeries, active, loading, error, newCount, setNewCount, refresh } =
    useActivityData({ api, query, newest, tsWindow, tsBucket, t, onUnauthorized });

  // Load a capture detail when a row's View action selects it. Same monotonic
  // request-id guard + 401 handling as useActivityData's load(); a stale
  // response is dropped.
  useEffect(() => {
    if (!detailRow) return;
    const id = detailRow.id;
    const myId = ++detailReqIdRef.current;
    setDetail(null);
    setDetailError('');
    setDetailLoading(true);
    void (async () => {
      try {
        const resp = await api.captureDetail(id);
        if (detailReqIdRef.current !== myId) return;
        setDetail(resp);
      } catch (err) {
        if (detailReqIdRef.current !== myId) return;
        if (err instanceof PortalApiError && err.status === 401) {
          onUnauthorizedRef.current();
          return;
        }
        setDetailError(
          err instanceof PortalApiError ? err.message : formatPortalError(err, tRef.current),
        );
      } finally {
        if (detailReqIdRef.current === myId) setDetailLoading(false);
      }
    })();
  }, [detailRow, api]);

  // Deletes the capture blob for the open row's usage event (metadata stays —
  // separate tables, FK only). A silent refetch flips has_capture to false so
  // the View action disappears without a full-page loading flash.
  const removeCapture = useCallback(async () => {
    if (!detailRow) return;
    try {
      await api.deleteCapture(detailRow.id);
      setConfirmingDeleteCapture(false);
      setDetailRow(null);
      void refresh({ silent: true });
    } catch (err) {
      if (err instanceof PortalApiError && err.status === 401) {
        onUnauthorizedRef.current();
        return;
      }
      // Surface via toast (Snackbar renders ABOVE the still-open Capture/Confirm
      // modals) — the page-level Alert would render behind the modal backdrops and
      // be invisible. Matches every other ConfirmDialog delete flow (TokenList,
      // ServerList, MappingSection, ApplicationSection).
      showError(formatPortalError(err, tRef.current));
    }
  }, [api, detailRow, refresh, showError]);

  // Owner-only secret toggle for the open capture. Mirrors removeCapture: flip the
  // flag, close the dialog, then silent refetch so the row's presence signal
  // (has_capture vs capture_locked) updates without a full-page loading flash.
  const toggleCaptureSecret = useCallback(
    async (next: boolean) => {
      if (!detailRow) return;
      try {
        await api.setCaptureSecret(detailRow.id, next);
        setDetailRow(null);
        void refresh({ silent: true });
      } catch (err) {
        if (err instanceof PortalApiError && err.status === 401) {
          onUnauthorizedRef.current();
          return;
        }
        showError(formatPortalError(err, tRef.current));
      }
    },
    [api, detailRow, refresh, showError],
  );

  // hidden/order/ownerDisplay/timeDisplay persist via usePreference (profile).
  // Scope + specific-user/token filters + ts window/bucket remain browser-local.
  useEffect(() => {
    writeScope(scope);
  }, [scope]);
  useEffect(() => {
    writeFilterUser(selectedUser);
  }, [selectedUser]);
  useEffect(() => {
    writeFilterToken(selectedToken);
  }, [selectedToken]);
  useEffect(() => {
    writeTsWindow(tsWindow);
  }, [tsWindow]);
  useEffect(() => {
    writeTsBucket(tsBucket);
  }, [tsBucket]);

  // USD-per-EUR conversion factor, fetched once on mount (best-effort; on error
  // it stays 0, which availableUnits() treats as "USD unavailable" — Euro-only).
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const resp = await api.getCurrency();
        if (!cancelled) setCurrencyFactor(resp.usd_per_eur);
      } catch {
        /* best-effort: keep 0 (USD unavailable) */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Admin user list for the "specific user" dropdown (best-effort; on error the
  // dropdown just stays empty). Fetched once per admin mount.
  useEffect(() => {
    if (!isAdmin) return;
    let cancelled = false;
    void (async () => {
      try {
        const resp = await api.adminUsers();
        if (!cancelled) setUserOptions(resp.data);
      } catch {
        /* best-effort */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, isAdmin]);

  // Token list for the token dropdown: own tokens (own scope) or the chosen user's
  // tokens (specific-user scope). Both already include the chat-session pseudo row.
  useEffect(() => {
    if (!showTokenFilter) {
      setTokenOptions([]);
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const resp =
          effectiveScope === 'user' ? await api.userTokens(selectedUser) : await api.tokens();
        if (!cancelled) {
          setTokenOptions(resp.data);
          // Drop a token selection that isn't in the freshly loaded set (e.g.
          // persisted from an earlier scope/user session). Guards the mount path
          // that a synchronous scope-change reset can't reach.
          setSelectedToken((cur) =>
            cur === '' || resp.data.some((tk) => tk.id === cur) ? cur : '',
          );
        }
      } catch {
        if (!cancelled) setTokenOptions([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, showTokenFilter, effectiveScope, selectedUser]);

  // `owner` is scope-gated: present in the order always, but rendered / listed /
  // toggleable only in the all-scope view.
  const showOwnerColumn = effectiveScope === 'all';
  // All columns in the persisted (drag-reordered) order, scope-gated (for the
  // column menu + ←/→ reorder). visibleColumns additionally drops hidden ones.
  const orderedColumns = useMemo(() => {
    const byId = new Map(ACTIVITY_COLUMNS.map((c) => [c.id, c]));
    return columnOrder
      .map((id) => byId.get(id))
      .filter(
        (c): c is (typeof ACTIVITY_COLUMNS)[number] =>
          Boolean(c) && (c!.id !== 'owner' || showOwnerColumn),
      );
  }, [columnOrder, showOwnerColumn]);
  const visibleColumns = useMemo(
    () => orderedColumns.filter((c) => !hidden.includes(c.id)),
    [orderedColumns, hidden],
  );

  function resetToPageOne(action: () => void) {
    action();
    setPage(1);
  }
  function onSort(columnId: string) {
    if (sort === columnId) {
      setOrder((o) => (o === 'asc' ? 'desc' : 'asc'));
    } else {
      setSort(columnId);
      setOrder('asc');
    }
    setPage(1);
  }

  const rows = pageData?.data ?? [];
  const total = pageData?.total ?? 0;

  const userSelectOptions: SearchableOption[] = [
    { value: '', label: t.activityFilterAll },
    ...userOptions.map((u) => ({ value: u.id, label: u.display_name || u.email })),
  ];
  const tokenSelectOptions: SearchableOption[] = [
    { value: '', label: t.activityFilterAll },
    ...tokenOptions.map((tok) => ({
      value: tok.id,
      label: tok.is_chat_session ? t.activityActiveSession : tok.name,
    })),
  ];

  // The persisted costUnit may be a USD unit from an earlier session where USD
  // was available; if currencyFactor has since dropped to <= 0 (or hasn't loaded
  // yet), fall back to eur_cent for display/wiring WITHOUT overwriting the stored
  // preference (so it's honored again once USD becomes available).
  const costUnitOptions = availableUnits(currencyFactor);
  const effectiveCostUnit: CurrencyUnit = costUnitOptions.includes(costUnit)
    ? costUnit
    : 'eur_cent';

  return (
    <>
      <PageTitle title={t.usage} subtitle={t.usageIntro} />

      <Box sx={{ mb: 3, display: 'flex', flexWrap: 'wrap', gap: 2, alignItems: 'flex-end' }}>
        {isAdmin && (
          <Box sx={{ minWidth: 200, maxWidth: 260 }}>
            <SelectField
              id="activity-scope"
              label={t.activityScopeLabel}
              value={scope}
              onChange={(e) =>
                resetToPageOne(() => {
                  // Clear the user/token selection on any scope switch so a token
                  // (or user) belonging to a different principal can't survive into
                  // the fetches and silently narrow every section to zero rows.
                  setScope(e.target.value as ActivityScope);
                  setSelectedUser('');
                  setSelectedToken('');
                })
              }
            >
              <option value="own">{t.activityScopeOwn}</option>
              <option value="user">{t.activityScopeSpecificUser}</option>
              <option value="all">{t.activityScopeAll}</option>
            </SelectField>
          </Box>
        )}
        {effectiveScope === 'user' && (
          <Box sx={{ minWidth: 220, maxWidth: 300 }}>
            <SearchableSelect
              id="activity-filter-user"
              label={t.activityUserFilterLabel}
              value={selectedUser}
              options={userSelectOptions}
              onChange={(v) =>
                resetToPageOne(() => {
                  setSelectedUser(v);
                  setSelectedToken('');
                })
              }
            />
          </Box>
        )}
        {showTokenFilter && (
          <Box sx={{ minWidth: 220, maxWidth: 300 }}>
            <SearchableSelect
              id="activity-filter-token"
              label={t.activityTokenFilterLabel}
              value={selectedToken}
              options={tokenSelectOptions}
              onChange={(v) => resetToPageOne(() => setSelectedToken(v))}
            />
          </Box>
        )}
        {/* Drives both the Usage-list cost column (ActivityTable) and the
            aggregate cost tile (StatTiles) below; persisted like timeDisplay. */}
        <Box sx={{ minWidth: 180, maxWidth: 240 }}>
          <SelectField
            id="activity-cost-unit"
            label={t.activityCostUnit}
            value={effectiveCostUnit}
            onChange={(e) => setCostUnit(e.target.value as CurrencyUnit)}
          >
            {costUnitOptions.map((u) => (
              <option key={u} value={u}>
                {unitLabel(t, u)}
              </option>
            ))}
          </SelectField>
        </Box>
      </Box>

      {stats && (
        <>
          <Box sx={{ display: 'flex', justifyContent: 'flex-end', mb: 1 }}>
            <SettingsMenu
              items={ACTIVITY_TILES.map((x) => ({
                id: x.id,
                label: t[x.labelKey as keyof typeof t] as string,
              }))}
              hidden={tilesHidden}
              order={tileOrder}
              onToggle={toggleTile}
              onReorder={reorderTile}
              onReset={resetTiles}
              buttonLabel={t.activityTilesButton}
              title={t.activityTilesTitle}
              resetLabel={t.listColumnsReset}
              moveLeftLabel={t.listColumnMoveLeft}
              moveRightLabel={t.listColumnMoveRight}
            />
          </Box>
          <StatTiles
            t={t}
            totals={stats.totals}
            order={tileOrder}
            hidden={tilesHidden}
            runningCount={active.length}
            costUnit={effectiveCostUnit}
            currencyFactor={currencyFactor}
          />

          <ActivityCharts
            t={t}
            stats={stats}
            timeSeries={timeSeries}
            tsWindow={tsWindow}
            tsBucket={tsBucket}
            onTsWindow={setTsWindow}
            onTsBucket={setTsBucket}
            collapsed={chartsCollapsed}
            onToggleCollapsed={() => setChartsCollapsed((v) => !v)}
          />
        </>
      )}

      <ActiveRequestsPanel t={t} active={active} effectiveScope={effectiveScope} />

      <Panel
        titleId="usage-heading"
        title={t.usageTableTitle}
        actions={
          <ActivityToolbar
            t={t}
            range={range}
            onRange={(v) => resetToPageOne(() => setRange(v))}
            customFrom={customRange.from}
            customTo={customRange.to}
            onCustomFrom={(v) => resetToPageOne(() => setCustomRange((p) => ({ ...p, from: v })))}
            onCustomTo={(v) => resetToPageOne(() => setCustomRange((p) => ({ ...p, to: v })))}
            groupChain={effectiveChain}
            onGroupChain={(v) => resetToPageOne(() => setGroupChain(v))}
            onRefresh={() => void refresh({ silent: true })}
          />
        }
      >
        {error && (
          <Alert severity="error" role="alert" sx={{ mb: 2 }}>
            {t.portalError}: {error}
          </Alert>
        )}
        {effectiveChain.length === 0 && !newest && newCount > 0 && (
          <Box sx={{ mb: 1.5 }}>
            <Button
              variant="outlined"
              size="small"
              onClick={() => {
                setNewCount(0);
                void refresh({ silent: true });
              }}
            >
              {t.activityNewRequests(newCount)}
            </Button>
          </Box>
        )}
        {effectiveChain.length > 0 ? (
          <ActivityGroups
            t={t}
            api={api}
            query={query}
            chain={effectiveChain}
            costUnit={effectiveCostUnit}
            currencyFactor={currencyFactor}
            timeDisplay={timeDisplay}
          />
        ) : (
          <ActivityTable
            t={t}
            rows={rows}
            columns={visibleColumns}
            ownerDisplay={ownerDisplay}
            sort={sort}
            order={order}
            onSort={onSort}
            page={page}
            limit={limit}
            total={total}
            onPageChange={setPage}
            onLimitChange={(l) => resetToPageOne(() => setLimit(l))}
            isEmpty={rows.length === 0}
            emptyLabel={loading ? t.loading : t.activityEmpty}
            onView={setDetailRow}
            q={q}
            onQChange={(v) => resetToPageOne(() => setQ(v))}
            textFilters={textFilters}
            onTextFilter={(key, value) =>
              resetToPageOne(() => setTextFilters((prev) => ({ ...prev, [key]: value })))
            }
            filterStatus={status}
            onFilterStatus={(v) => resetToPageOne(() => setStatus(v))}
            filterStream={stream}
            onFilterStream={(v) => resetToPageOne(() => setStream(v))}
            numericFilters={numeric}
            onNumericFilter={(col, next) =>
              resetToPageOne(() => setNumeric((prev) => ({ ...prev, [col]: next })))
            }
            timeFrom={timeFrom}
            timeTo={timeTo}
            onTimeFilter={(next) =>
              resetToPageOne(() => {
                setTimeFrom(next.from);
                setTimeTo(next.to);
              })
            }
            timeDisplay={timeDisplay}
            costUnit={effectiveCostUnit}
            currencyFactor={currencyFactor}
            onOpenColumns={(e: MouseEvent<HTMLElement>) => setColumnsAnchor(e.currentTarget)}
            onReorderColumn={reorderColumn}
          />
        )}
      </Panel>

      <ColumnMenu
        t={t}
        open={Boolean(columnsAnchor)}
        anchorEl={columnsAnchor}
        onClose={() => setColumnsAnchor(null)}
        columns={orderedColumns}
        hidden={hidden}
        onToggle={toggleColumn}
        onReorder={reorderColumn}
        onReset={resetColumns}
        scope={effectiveScope}
        ownerDisplay={ownerDisplay}
        onOwnerDisplayChange={(value) => {
          setOwnerDisplay(value);
          setColumnsAnchor(null);
        }}
        timeDisplay={timeDisplay}
        onTimeDisplayChange={setTimeDisplay}
        moveLeftLabel={t.listColumnMoveLeft}
        moveRightLabel={t.listColumnMoveRight}
      />

      <CaptureDialog
        t={t}
        open={Boolean(detailRow)}
        onClose={() => setDetailRow(null)}
        detail={detail}
        loading={detailLoading}
        error={detailError}
        onRequestDelete={() => setConfirmingDeleteCapture(true)}
        onToggleSecret={(next) => void toggleCaptureSecret(next)}
      />

      <ConfirmDialog
        open={confirmingDeleteCapture}
        title={t.captureDeleteConfirm}
        confirmLabel={t.captureDelete}
        cancelLabel={t.tokenActionCancel}
        onConfirm={() => void removeCapture()}
        onCancel={() => setConfirmingDeleteCapture(false)}
      />
    </>
  );
}
