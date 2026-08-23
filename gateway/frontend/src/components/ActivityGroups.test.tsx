// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActivityGroups } from './ActivityGroups';
import { formatEnergyWh } from './StatTiles';
import { formatCost } from '../currency';
import { messages } from '../i18n';
import {
  buildQueryString,
  type ActivityQuery,
  type UsageEvent,
  type UsageGroupRow,
  type UsagePage,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

function makeGroup(overrides: Partial<UsageGroupRow> = {}): UsageGroupRow {
  return {
    key: 'srv-a',
    key_label: 'GPU A',
    count: 12,
    error_count: 1,
    input_tokens: 100,
    output_tokens: 200,
    total_tokens: 300,
    cached_tokens: 0,
    cache_write_tokens: 0,
    energy_wh: 42.5,
    cost_eur: 1.2345,
    first_at: '2026-07-10T12:00:00Z',
    last_at: '2026-07-10T12:30:00Z',
    ...overrides,
  };
}

function makeMember(overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id: 'req_1',
    user_id: 'usr_1',
    token_id: 'tok_1',
    api_flavor: 'portal_chat',
    model: 'qwen-coder',
    provider: 'mock',
    host: 'mock-host',
    input_tokens: 2,
    output_tokens: 6,
    total_tokens: 8,
    latency_ms: 14,
    status: 'success',
    created_at: '2026-07-10T12:01:00Z',
    cached_tokens: 0,
    cache_write_tokens: 0,
    prompt_per_second: 12.5,
    tokens_per_second: 40,
    http_status: 200,
    content_type: 'application/json',
    req_path: '/v1/chat/completions',
    provider_path: '/v1/chat/completions',
    provider_model: 'qwen2.5',
    stream: true,
    token_name: 'Dev Token',
    server_name: 'GPU A',
    ...overrides,
  } as UsageEvent;
}

function makePage(rows: UsageEvent[], overrides: Partial<UsagePage> = {}): UsagePage {
  return { data: rows, page: 1, limit: 25, total: rows.length, total_pages: 1, ...overrides };
}

type Over = {
  usageGroups?: PortalApi['usageGroups'];
  activity?: PortalApi['activity'];
};

function makeApi(over: Over = {}) {
  const api = {
    usageGroups:
      over.usageGroups ?? vi.fn(async () => ({ data: [makeGroup()], group_by: 'server' })),
    activity: over.activity ?? vi.fn(async () => makePage([makeMember()])),
  };
  return api;
}

const baseQuery: ActivityQuery = {
  page: 1,
  limit: 25,
  sort: 'created_at',
  order: 'desc',
  range: '30d',
  scope: 'own',
};

function renderGroups(over: Over = {}, chain = ['server']) {
  const api = makeApi(over);
  render(
    <ActivityGroups
      t={t}
      api={api}
      query={baseQuery}
      chain={chain}
      costUnit="eur_cent"
      currencyFactor={0}
      timeDisplay="absolute"
    />,
  );
  return api;
}

afterEach(cleanup);

describe('ActivityGroups', () => {
  it('renders aggregate rows with label, count and formatted energy/cost', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [
        makeGroup({
          key: 'srv-a',
          key_label: 'GPU A',
          count: 12,
          energy_wh: 42.5,
          cost_eur: 1.2345,
        }),
        makeGroup({ key: 'srv-b', key_label: 'GPU B', count: 7, energy_wh: 1500, cost_eur: 9.5 }),
      ],
      group_by: 'server',
    }));
    renderGroups({ usageGroups });

    expect(await screen.findByText('GPU A')).toBeInTheDocument();
    expect(screen.getByText('GPU B')).toBeInTheDocument();
    expect(screen.getByText('12')).toBeInTheDocument();
    expect(screen.getByText('7')).toBeInTheDocument();
    // Energy/cost use the shared formatters (kWh threshold, cost unit).
    expect(screen.getByText(formatEnergyWh(42.5))).toBeInTheDocument();
    expect(screen.getByText(formatEnergyWh(1500))).toBeInTheDocument();
    expect(screen.getByText(formatCost(1.2345, 'eur_cent', 0))).toBeInTheDocument();
    expect(screen.getByText(formatCost(9.5, 'eur_cent', 0))).toBeInTheDocument();
    expect(usageGroups).toHaveBeenCalledWith(expect.objectContaining({ group_by: 'server' }));
  });

  it('expands a group by model with model_exact and renders member rows', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: 'qwen-coder', key_label: 'qwen-coder', count: 3 })],
      group_by: 'model',
    }));
    const activity = vi.fn(async () => makePage([makeMember({ id: 'm1', model: 'qwen-coder' })]));
    const api = renderGroups({ usageGroups, activity }, ['model']);

    fireEvent.click(await screen.findByText('qwen-coder'));

    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    const call = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)!;
    expect(call[0]).toMatchObject({ model_exact: 'qwen-coder', page: 1 });
    // Member row rendered inline: its server name (distinct from the group label)
    // appears in the inner table.
    expect(await screen.findByText('GPU A')).toBeInTheDocument();
  });

  it('uses server_exact when grouping by server', async () => {
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = renderGroups({ activity }, ['server']);
    fireEvent.click(await screen.findByText('GPU A'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    expect(
      (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      server_exact: 'srv-a',
    });
  });

  it('uses session_id_exact when grouping by session', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: 'sess-1', key_label: 'sess-1' })],
      group_by: 'session',
    }));
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = renderGroups({ usageGroups, activity }, ['session']);
    fireEvent.click(await screen.findByText('sess-1'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    expect(
      (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      session_id_exact: 'sess-1',
    });
  });

  it('uses user_id + scope:all when grouping by user', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: 'usr-9', key_label: 'Alice' })],
      group_by: 'user',
    }));
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = renderGroups({ usageGroups, activity }, ['user']);
    fireEvent.click(await screen.findByText('Alice'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    expect(
      (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      user_id: 'usr-9',
      scope: 'all',
    });
  });

  it('uses service_id when grouping by service (Phase 1 service accounts)', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: 'svc_1', key_label: 'Nightly Batch' })],
      group_by: 'service',
    }));
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = renderGroups({ usageGroups, activity }, ['service']);
    expect(await screen.findByText('Nightly Batch')).toBeInTheDocument();
    expect(usageGroups).toHaveBeenCalledWith(expect.objectContaining({ group_by: 'service' }));

    fireEvent.click(screen.getByText('Nightly Batch'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    expect(
      (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      service_id: 'svc_1',
    });
  });

  it("labels the empty-key service group as 'no service' (usage not attributed to a Service Account)", async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: '', key_label: '', count: 4 })],
      group_by: 'service',
    }));
    renderGroups({ usageGroups }, ['service']);
    expect(await screen.findByText(t.activityGroupServiceNone)).toBeInTheDocument();
  });

  it('uses project_id_exact when grouping by project (spec: 2026-08-08-projects-design.md §7)', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: 'proj_1', key_label: 'Rocket Launch' })],
      group_by: 'project',
    }));
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = renderGroups({ usageGroups, activity }, ['project']);
    expect(await screen.findByText('Rocket Launch')).toBeInTheDocument();
    expect(usageGroups).toHaveBeenCalledWith(expect.objectContaining({ group_by: 'project' }));

    fireEvent.click(screen.getByText('Rocket Launch'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    expect(
      (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      project_id_exact: 'proj_1',
    });
  });

  it("expands an empty-key project group with the __empty__ sentinel and labels it 'no project'", async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: '', key_label: '', count: 4 })],
      group_by: 'project',
    }));
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = renderGroups({ usageGroups, activity }, ['project']);
    expect(await screen.findByText(t.activityGroupProjectNone)).toBeInTheDocument();

    fireEvent.click(screen.getByText(t.activityGroupProjectNone));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    const arg = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0];
    expect(arg).toMatchObject({ project_id_exact: '__empty__' });
    expect(buildQueryString(arg as Record<string, string | number | undefined>)).toContain(
      'project_id_exact=__empty__',
    );
  });

  it('renders the session label for an empty token key and expands with token_id', async () => {
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: '', key_label: '', count: 4 })],
      group_by: 'token',
    }));
    const activity = vi.fn(async () => makePage([makeMember({ token_id: '' })]));
    const api = renderGroups({ usageGroups, activity }, ['token']);

    const label = await screen.findByText(t.activityGroupTokenNone);
    expect(label).toBeInTheDocument();
    fireEvent.click(label);
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    const arg = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(
      -1,
    )![0] as ActivityQuery;
    // The empty token key expands via the __none__ sentinel (NOT token_id:"" — that
    // would be dropped by buildQueryString and match everything).
    expect(arg).toMatchObject({ token_id: '__none__' });
    // Wire-level: the sentinel survives buildQueryString to the URL query (an empty
    // value would be stripped — the bug this fix closes).
    expect(buildQueryString(arg as Record<string, string | number | undefined>)).toContain(
      'token_id=__none__',
    );
  });

  it('expands an empty session key with the __empty__ sentinel that survives to the wire', async () => {
    // key === "" is the session-less bucket; a distinctive key_label just makes the
    // row clickable (exactFilter keys on row.key, not the label).
    const usageGroups = vi.fn(async () => ({
      data: [makeGroup({ key: '', key_label: '(no session)' })],
      group_by: 'session',
    }));
    const activity = vi.fn(async () =>
      makePage([makeMember({ session_id: '' } as Partial<UsageEvent>)]),
    );
    const api = renderGroups({ usageGroups, activity }, ['session']);

    fireEvent.click(await screen.findByText('(no session)'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    const arg = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(
      -1,
    )![0] as ActivityQuery;
    expect(arg).toMatchObject({ session_id_exact: '__empty__' });
    expect(buildQueryString(arg as Record<string, string | number | undefined>)).toContain(
      'session_id_exact=__empty__',
    );
  });

  it('shows the empty state when there are no groups', async () => {
    const usageGroups = vi.fn(async () => ({ data: [], group_by: 'server' }));
    renderGroups({ usageGroups });
    expect(await screen.findByText(t.activityEmpty)).toBeInTheDocument();
  });

  it('shows the Cached column by default and hides Cache-Write by default', async () => {
    renderGroups();
    await screen.findByText('GPU A');
    // columnheader role targets the header cell specifically (a menu checkbox with
    // the same label would otherwise collide).
    expect(screen.getByRole('columnheader', { name: t.activityColCached })).toBeInTheDocument();
    expect(
      screen.queryByRole('columnheader', { name: t.activityColCacheWrite }),
    ).not.toBeInTheDocument();
  });

  it('toggles a column off via the settings menu', async () => {
    renderGroups();
    await screen.findByText('GPU A');
    expect(screen.getByRole('columnheader', { name: t.activityColCached })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    const cachedCheckbox = screen.getByRole('checkbox', { name: t.activityColCached });
    expect(cachedCheckbox).toBeChecked();
    fireEvent.click(cachedCheckbox);
    await waitFor(() =>
      expect(
        screen.queryByRole('columnheader', { name: t.activityColCached }),
      ).not.toBeInTheDocument(),
    );
  });

  it("recurses: expanding a level renders the next dimension's sub-groups", async () => {
    // Level 0 groups by user; level 1 groups by server within the picked user.
    const usageGroups = vi.fn(async (q: ActivityQuery & { group_by?: string }) => {
      if (q.group_by === 'user')
        return { data: [makeGroup({ key: 'usr-9', key_label: 'Alice' })], group_by: 'user' };
      // level-1 call MUST carry the ancestor user filter.
      expect(q).toMatchObject({ group_by: 'server', user_id: 'usr-9', scope: 'all' });
      return { data: [makeGroup({ key: 'srv-a', key_label: 'GPU A' })], group_by: 'server' };
    });
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = makeApi({
      usageGroups: usageGroups as unknown as PortalApi['usageGroups'],
      activity,
    });
    render(
      <ActivityGroups
        t={t}
        api={api}
        query={baseQuery}
        chain={['user', 'server']}
        costUnit="eur_cent"
        currencyFactor={0}
        timeDisplay="absolute"
      />,
    );

    // Expand the user row -> the nested server sub-table appears (not member requests).
    fireEvent.click(await screen.findByText('Alice'));
    expect(await screen.findByText('GPU A')).toBeInTheDocument();
    // No member fetch happened at the non-last level.
    expect(api.activity).not.toHaveBeenCalled();
    expect(
      (usageGroups as unknown as { mock: { calls: unknown[][] } }).mock.calls.length,
    ).toBeGreaterThanOrEqual(2);
  });

  it('recurses to member requests on the deepest level with all ancestor filters', async () => {
    const usageGroups = vi.fn(async (q: ActivityQuery & { group_by?: string }) => {
      if (q.group_by === 'user')
        return { data: [makeGroup({ key: 'usr-9', key_label: 'Alice' })], group_by: 'user' };
      return { data: [makeGroup({ key: 'srv-a', key_label: 'GPU A' })], group_by: 'server' };
    });
    const activity = vi.fn(async () => makePage([makeMember({ id: 'm1' })]));
    const api = makeApi({
      usageGroups: usageGroups as unknown as PortalApi['usageGroups'],
      activity,
    });
    render(
      <ActivityGroups
        t={t}
        api={api}
        query={baseQuery}
        chain={['user', 'server']}
        costUnit="eur_cent"
        currencyFactor={0}
        timeDisplay="absolute"
      />,
    );
    fireEvent.click(await screen.findByText('Alice'));
    fireEvent.click(await screen.findByText('GPU A'));
    await waitFor(() => expect(api.activity).toHaveBeenCalled());
    // The deepest expand carries BOTH ancestor keys.
    expect(
      (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      user_id: 'usr-9',
      server_exact: 'srv-a',
    });
  });

  it('paginates the expanded member table across pages, disabling Prev/Next at the boundaries', async () => {
    // Page-dependent responses: page 1 -> member m1 (2 total pages); page 2 -> m2.
    const activity = vi.fn(async (q?: ActivityQuery) =>
      q?.page === 2
        ? makePage([makeMember({ id: 'm2', model: 'llama' })], {
            page: 2,
            total: 2,
            total_pages: 2,
          })
        : makePage([makeMember({ id: 'm1', model: 'qwen-coder' })], {
            page: 1,
            total: 2,
            total_pages: 2,
          }),
    );
    renderGroups({ activity }, ['server']);

    fireEvent.click(await screen.findByText('GPU A'));
    await waitFor(() => expect(activity).toHaveBeenCalledTimes(1));
    expect(await screen.findByText('qwen-coder')).toBeInTheDocument();

    // Page 1 of 2: Prev disabled (already at the first page), Next enabled.
    const prevBtn = await screen.findByRole('button', { name: t.activityPrevPage });
    const nextBtn = await screen.findByRole('button', { name: t.activityNextPage });
    expect(prevBtn).toBeDisabled();
    expect(nextBtn).not.toBeDisabled();
    expect(screen.getByText(`1 ${t.listRangeOf} 2 · 2 ${t.listRowsSuffix}`)).toBeInTheDocument();

    fireEvent.click(nextBtn);
    await waitFor(() => expect(activity).toHaveBeenCalledTimes(2));
    expect(activity.mock.calls.at(-1)![0]).toMatchObject({ page: 2 });
    expect(await screen.findByText('llama')).toBeInTheDocument();
    expect(screen.queryByText('qwen-coder')).toBeNull();

    // Page 2 of 2: Prev enabled, Next disabled (memberPage >= memberPages).
    expect(prevBtn).not.toBeDisabled();
    expect(nextBtn).toBeDisabled();

    fireEvent.click(prevBtn);
    await waitFor(() => expect(activity).toHaveBeenCalledTimes(3));
    expect(activity.mock.calls.at(-1)![0]).toMatchObject({ page: 1 });
    expect(await screen.findByText('qwen-coder')).toBeInTheDocument();
    expect(prevBtn).toBeDisabled();
  });

  it("keeps a nested child's query identity stable across an ancestor re-render with a deep-equal query object (no refetch/collapse)", async () => {
    // Reproduces the bug: Activity.tsx's 1s relative-time ticker re-renders the
    // whole tree, recreating `query={{ ...query, ...exactFilter(...) }}` as a
    // FRESH object every tick for a nested (level>0) ActivityGroups. Without
    // stabilizing the query identity by VALUE, the fetch effects (keyed on
    // `query` by reference) refire every tick: loading flicker + a fresh
    // usageGroups call + `setOpenKey(null)` collapsing any deeper expansion.
    const usageGroups = vi.fn(async (q: ActivityQuery & { group_by?: string }) => {
      if (q.group_by === 'user')
        return { data: [makeGroup({ key: 'usr-9', key_label: 'Alice' })], group_by: 'user' };
      return { data: [makeGroup({ key: 'srv-a', key_label: 'GPU A' })], group_by: 'server' };
    });
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = makeApi({
      usageGroups: usageGroups as unknown as PortalApi['usageGroups'],
      activity,
    });
    const { rerender } = render(
      <ActivityGroups
        t={t}
        api={api}
        query={baseQuery}
        chain={['user', 'server']}
        costUnit="eur_cent"
        currencyFactor={0}
        timeDisplay="absolute"
      />,
    );

    // Expand the user row -> the nested server sub-table appears.
    fireEvent.click(await screen.findByText('Alice'));
    expect(await screen.findByText('GPU A')).toBeInTheDocument();

    const callsBefore = (usageGroups as unknown as { mock: { calls: unknown[][] } }).mock.calls
      .length;

    // Re-render with a NEW but deep-equal query object (simulates the ticker).
    rerender(
      <ActivityGroups
        t={t}
        api={api}
        query={{ ...baseQuery }}
        chain={['user', 'server']}
        costUnit="eur_cent"
        currencyFactor={0}
        timeDisplay="absolute"
      />,
    );

    // Give any effect a tick to (wrongly) fire before asserting it did not.
    await new Promise((resolve) => setTimeout(resolve, 0));

    // No refetch at ANY level.
    expect((usageGroups as unknown as { mock: { calls: unknown[][] } }).mock.calls).toHaveLength(
      callsBefore,
    );
    // The nested expansion was NOT collapsed.
    expect(screen.getByText('GPU A')).toBeInTheDocument();
  });

  it('renders the settings bar only at the top level', async () => {
    const usageGroups = vi.fn(async (q: ActivityQuery & { group_by?: string }) => {
      if (q.group_by === 'user')
        return { data: [makeGroup({ key: 'usr-9', key_label: 'Alice' })], group_by: 'user' };
      return { data: [makeGroup({ key: 'srv-a', key_label: 'GPU A' })], group_by: 'server' };
    });
    const activity = vi.fn(async () => makePage([makeMember()]));
    const api = makeApi({
      usageGroups: usageGroups as unknown as PortalApi['usageGroups'],
      activity,
    });
    render(
      <ActivityGroups
        t={t}
        api={api}
        query={baseQuery}
        chain={['user', 'server']}
        costUnit="eur_cent"
        currencyFactor={0}
        timeDisplay="absolute"
      />,
    );
    fireEvent.click(await screen.findByText('Alice'));
    await screen.findByText('GPU A');
    // Exactly one column-settings trigger (top level); the nested server
    // sub-table (showSettings=false) must add none.
    expect(screen.getAllByRole('button', { name: t.listColumns })).toHaveLength(1);
  });
});
