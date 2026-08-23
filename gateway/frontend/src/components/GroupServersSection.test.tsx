// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, cleanup, act, within } from '@testing-library/react';
import { GroupServersSection } from './GroupServersSection';
import { messages } from '../i18n';
import type { GroupModelServerRow, ModelOption } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(() => cleanup());

const group: ModelOption = {
  id: 'fast-group',
  display_name: 'fast-group',
  flavors: [],
  is_group: true,
};

function makeRows(): GroupModelServerRow[] {
  return [
    {
      model: 'qwen-coder',
      server_id: 'srv-a',
      server_name: 'GPU-Box-A',
      application_id: 'app-a',
      mapping_id: 'map-a',
      loaded: true,
      can_load: true,
      gen_tokens_per_second: 42.5,
      prompt_tokens_per_second: 210,
      load_time_ms: 1200,
      context_size: 32768,
      max_concurrency: 8,
      recommended_concurrency: 4,
      gen_tokens_per_second_at_capacity: 30,
      is_mtp: false,
      metrics_source: 'benchmark',
      metrics_updated_at: '2026-08-01T10:00:00Z',
      priority: 1,
    },
    {
      model: 'llama3',
      server_id: 'srv-b',
      server_name: 'GPU-Box-B',
      application_id: 'app-b',
      mapping_id: 'map-b',
      loaded: false,
      can_load: false,
      gen_tokens_per_second: 11.1,
      prompt_tokens_per_second: 90,
      load_time_ms: 800,
      context_size: 16384,
      max_concurrency: 4,
      recommended_concurrency: 2,
      gen_tokens_per_second_at_capacity: 9,
      is_mtp: false,
      metrics_source: 'manual',
      metrics_updated_at: null,
      priority: 2,
    },
  ];
}

function makeApi(
  overrides: Partial<Pick<PortalApi, 'modelGroupServers'>> = {},
): Pick<PortalApi, 'modelGroupServers'> {
  return {
    modelGroupServers: vi.fn().mockResolvedValue(makeRows()),
    ...overrides,
  };
}

function rowFor(serverName: string): HTMLElement {
  return screen.getByText(serverName).closest('tr')!;
}

describe('GroupServersSection', () => {
  it('renders the Prio, Model, and Server columns for every offering row', async () => {
    const api = makeApi();
    render(<GroupServersSection t={t} api={api} group={group} isAdmin pollIntervalMs={200} />);

    expect(await screen.findByText('GPU-Box-A')).toBeInTheDocument();
    expect(screen.getByText('GPU-Box-B')).toBeInTheDocument();
    expect(screen.getByText('qwen-coder')).toBeInTheDocument();
    expect(screen.getByText('llama3')).toBeInTheDocument();

    // Column headers.
    expect(screen.getByText(t.modelServerColPrio)).toBeInTheDocument();
    expect(screen.getByText(t.modelServerColModel)).toBeInTheDocument();
    expect(screen.getByText(t.modelServerColServer)).toBeInTheDocument();

    // Live Prio rank per row.
    expect(within(rowFor('GPU-Box-A')).getByText('1')).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-B')).getByText('2')).toBeInTheDocument();

    // The group-detail intro subtitle is shown.
    expect(screen.getByText(t.groupServersIntro)).toBeInTheDocument();
  });

  it("fetches by the group's id and re-polls the list live", async () => {
    // Fake timers own the clock here: the pre-poll assertions below are only
    // meaningful BEFORE the interval fires, and under real timers that window is
    // just `pollIntervalMs` of wall clock (a full-suite run under load overshoots
    // it and the poll lands first).
    vi.useFakeTimers();
    try {
      const rows = makeRows();
      const reranked = rows.map((r) =>
        r.mapping_id === 'map-b' ? { ...r, priority: 1 } : { ...r, priority: 2 },
      );
      const modelGroupServers = vi.fn().mockResolvedValueOnce(rows).mockResolvedValue(reranked);
      const api = makeApi({ modelGroupServers });
      render(<GroupServersSection t={t} api={api} group={group} isAdmin pollIntervalMs={50} />);

      // Flush ONLY the mount fetch's promise chain (.then/.catch/.finally) — no
      // clock advance, so the 50ms poll tick cannot fire yet. NOTE: do not use
      // findBy*/waitFor under fake timers here; RTL's async utilities advance fake
      // timers themselves and would fire the poll, re-introducing the race.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(modelGroupServers).toHaveBeenCalledWith('fast-group');
      expect(modelGroupServers).toHaveBeenCalledTimes(1);

      // Initial mount fetch: rowA=1, rowB=2 — now deterministically observable.
      expect(within(rowFor('GPU-Box-A')).getByText('1')).toBeInTheDocument();
      expect(within(rowFor('GPU-Box-B')).getByText('2')).toBeInTheDocument();

      // Exactly one poll tick re-fetches and re-renders the new ranking: the
      // interval IS the cause of the re-poll, not an incidental second fetch.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(50);
      });
      expect(modelGroupServers).toHaveBeenCalledTimes(2);
      expect(within(rowFor('GPU-Box-B')).getByText('1')).toBeInTheDocument();
      expect(within(rowFor('GPU-Box-A')).getByText('2')).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops re-polling after unmount', async () => {
    vi.useFakeTimers();
    try {
      const modelGroupServers = vi.fn().mockResolvedValue(makeRows());
      const api = makeApi({ modelGroupServers });
      const { unmount } = render(
        <GroupServersSection t={t} api={api} group={group} isAdmin pollIntervalMs={50} />,
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(modelGroupServers).toHaveBeenCalledTimes(1);

      unmount();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(50 * 5);
      });
      // clearInterval on unmount: the mount fetch stays the only call.
      expect(modelGroupServers).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("has no 'Laden' action (read-only view)", async () => {
    const api = makeApi();
    render(<GroupServersSection t={t} api={api} group={group} isAdmin pollIntervalMs={200} />);
    await screen.findByText('GPU-Box-A');
    expect(screen.queryByText(t.modelServerLoad)).toBeNull();
    expect(screen.queryByRole('button', { name: t.listRowMenu })).toBeNull();
  });
});
