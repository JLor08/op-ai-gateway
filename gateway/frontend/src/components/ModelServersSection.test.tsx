// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { ModelServersSection } from './ModelServersSection';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import {
  PortalApiError,
  type BenchmarkStatus,
  type ModelOption,
  type ModelServerRow,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(() => cleanup());

const model: ModelOption = { id: 'qwen-coder', display_name: 'qwen-coder', flavors: [] };

// rowA: loaded + can_load → Laden disabled (already loaded).
// rowB: not loaded, no permission → Laden disabled (owner/admin only).
// rowC: not loaded + can_load → Laden ENABLED.
function makeRows(): ModelServerRow[] {
  return [
    {
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
      priority: 3,
    },
    {
      server_id: 'srv-c',
      server_name: 'GPU-Box-C',
      application_id: 'app-c',
      mapping_id: 'map-c',
      loaded: false,
      can_load: true,
      gen_tokens_per_second: 33.3,
      prompt_tokens_per_second: 150,
      load_time_ms: 950,
      context_size: 8192,
      max_concurrency: 6,
      recommended_concurrency: 3,
      gen_tokens_per_second_at_capacity: 20,
      is_mtp: true,
      metrics_source: 'benchmark',
      metrics_updated_at: '2026-08-01T09:00:00Z',
      priority: 2,
    },
  ];
}

type OnData = (rows: ModelServerRow[]) => void;

type ModelServersSectionApi = Pick<
  PortalApi,
  'benchmarkStatus' | 'loadModel' | 'modelServers' | 'subscribeModelServers'
>;

function makeApi(overrides: Partial<ModelServersSectionApi> = {}): {
  api: ModelServersSectionApi;
  getOnData: () => OnData | null;
} {
  let onDataRef: OnData | null = null;
  const api: ModelServersSectionApi = {
    modelServers: vi.fn().mockResolvedValue(makeRows()),
    subscribeModelServers: vi.fn((_name: string, onData: OnData) => {
      onDataRef = onData;
      return () => {};
    }),
    loadModel: vi.fn(),
    benchmarkStatus: vi.fn(),
    ...overrides,
  };
  return { api, getOnData: () => onDataRef };
}

function renderSection(api: ModelServersSectionApi) {
  return render(
    <ToastProvider>
      <ModelServersSection t={t} api={api} model={model} isAdmin pollIntervalMs={1} />
    </ToastProvider>,
  );
}

function rowFor(serverName: string): HTMLElement {
  return screen.getByText(serverName).closest('tr')!;
}

// The single "Laden" action lives in the kebab (⋮) row menu (maxInlineActions=0)
// so a disabled item can surface its reason. Open a row's menu, close it again.
function openMenu(serverName: string) {
  fireEvent.click(within(rowFor(serverName)).getByRole('button', { name: t.listRowMenu }));
}
async function closeMenu() {
  fireEvent.keyDown(await screen.findByRole('menu'), { key: 'Escape' });
  await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
}
function loadItem(): Promise<HTMLElement> {
  return screen.findByRole('menuitem', { name: t.modelServerLoad });
}

describe('ModelServersSection', () => {
  it('renders every offering server with its metrics after mount', async () => {
    const { api } = makeApi();
    renderSection(api);
    expect(await screen.findByText('GPU-Box-A')).toBeInTheDocument();
    expect(screen.getByText('GPU-Box-B')).toBeInTheDocument();
    expect(screen.getByText('GPU-Box-C')).toBeInTheDocument();
    // A gen-tok/s metric value renders (rowA = 42.5).
    expect(screen.getByText('42.5')).toBeInTheDocument();
    // The live "Prio" rank renders for each row (rowA=1, rowC=2, rowB=3).
    expect(within(rowFor('GPU-Box-A')).getByText('1')).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-C')).getByText('2')).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-B')).getByText('3')).toBeInTheDocument();
  });

  it('re-ranks the Prio column live via the poll', async () => {
    // Fake timers own the clock here: the pre-poll assertions below are only
    // meaningful BEFORE the interval fires, and under real timers that window is
    // just `pollIntervalMs` of wall clock — which also has to absorb the MUI
    // render and findByText's own DOM polling, so a full-suite run under load
    // overshoots it and the poll lands first.
    vi.useFakeTimers();
    try {
      const rows = makeRows();
      // A later poll flips the ranking: rowC climbs to 1, rowA drops to 2.
      const reranked = rows.map((r) =>
        r.mapping_id === 'map-c'
          ? { ...r, priority: 1 }
          : r.mapping_id === 'map-a'
            ? { ...r, priority: 2 }
            : r,
      );
      const modelServers = vi.fn().mockResolvedValueOnce(rows).mockResolvedValue(reranked);
      const { api } = makeApi({ modelServers } as Partial<ModelServersSectionApi>);
      render(
        <ToastProvider>
          <ModelServersSection t={t} api={api} model={model} isAdmin pollIntervalMs={200} />
        </ToastProvider>,
      );

      // Flush ONLY the mount fetch's promise chain (.then/.catch/.finally) — no
      // clock advance, so the 200ms poll tick cannot fire yet. NOTE: do not use
      // findBy*/waitFor under fake timers here; RTL's async utilities advance fake
      // timers themselves and would fire the poll, re-introducing the race.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(modelServers).toHaveBeenCalledWith('qwen-coder');
      expect(modelServers).toHaveBeenCalledTimes(1);

      // Initial mount fetch: rowA=1, rowC=2 — now deterministically observable.
      expect(within(rowFor('GPU-Box-A')).getByText('1')).toBeInTheDocument();
      expect(within(rowFor('GPU-Box-C')).getByText('2')).toBeInTheDocument();

      // Exactly one poll tick re-fetches and re-renders the new ranking: the
      // interval IS the cause of the re-poll, not an incidental second fetch.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(200);
      });
      expect(modelServers).toHaveBeenCalledTimes(2);
      expect(within(rowFor('GPU-Box-C')).getByText('1')).toBeInTheDocument();
      expect(within(rowFor('GPU-Box-A')).getByText('2')).toBeInTheDocument();
      // The rest of the table is undisturbed by the re-poll.
      expect(within(rowFor('GPU-Box-B')).getByText('3')).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops re-polling after unmount', async () => {
    vi.useFakeTimers();
    try {
      const modelServers = vi.fn().mockResolvedValue(makeRows());
      const { api } = makeApi({ modelServers } as Partial<ModelServersSectionApi>);
      const { unmount } = render(
        <ToastProvider>
          <ModelServersSection t={t} api={api} model={model} isAdmin pollIntervalMs={200} />
        </ToastProvider>,
      );
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(modelServers).toHaveBeenCalledTimes(1);

      unmount();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(200 * 5);
      });
      // clearInterval on unmount: the mount fetch stays the only call.
      expect(modelServers).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reflects loaded-state and updates it live on an SSE frame', async () => {
    const { api, getOnData } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowA is loaded; rowC is not.
    expect(within(rowFor('GPU-Box-A')).getByText(t.tableModelLoaded)).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-C')).getByText(t.modelServerNotLoaded)).toBeInTheDocument();

    // A live update flips rowC to loaded.
    const updated = makeRows().map((r) => (r.mapping_id === 'map-c' ? { ...r, loaded: true } : r));
    act(() => getOnData()!(updated));
    expect(within(rowFor('GPU-Box-C')).getByText(t.tableModelLoaded)).toBeInTheDocument();
  });

  it('gates the Laden action on can_load + not-loaded, surfacing the reason on a disabled row', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowA: already loaded → the Laden menu item is present but disabled (the reason
    // is carried as its RowAction.title, which the menu renders via Tooltip+span —
    // only reachable because the single action collapses into the kebab menu).
    openMenu('GPU-Box-A');
    expect(await loadItem()).toHaveAttribute('aria-disabled', 'true');
    await closeMenu();

    // rowB: no permission → disabled.
    openMenu('GPU-Box-B');
    expect(await loadItem()).toHaveAttribute('aria-disabled', 'true');
    await closeMenu();

    // rowC: loadable + idle → enabled.
    openMenu('GPU-Box-C');
    expect(await loadItem()).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('loads a model and shows a success toast on completion', async () => {
    const loadModel = vi
      .fn()
      .mockResolvedValue({ running: true, server_id: 'srv-c' } as BenchmarkStatus);
    const benchmarkStatus = vi.fn().mockResolvedValue({
      running: false,
      server_id: 'srv-c',
      results: [{ mapping_id: 'map-c' }],
    } as unknown as BenchmarkStatus);
    const { api } = makeApi({ loadModel, benchmarkStatus } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    openMenu('GPU-Box-C');
    fireEvent.click(await loadItem());

    await waitFor(() => expect(loadModel).toHaveBeenCalledWith('map-c'));
    expect(await screen.findByText(t.modelServerLoadSuccess)).toBeInTheDocument();
  });

  it('surfaces a busy toast when the server is in use (409)', async () => {
    const loadModel = vi
      .fn()
      .mockRejectedValue(new PortalApiError(409, 'benchmark.server_in_use', 'busy'));
    const { api } = makeApi({ loadModel } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    openMenu('GPU-Box-C');
    fireEvent.click(await loadItem());

    expect(await screen.findByText(t.modelServerBusy)).toBeInTheDocument();
  });

  it("renders the Vision column (hidden by default) reflecting each row's vision_capable flag", async () => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
    const rows = makeRows().map((r) => ({ ...r, vision_capable: r.mapping_id === 'map-c' }));
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // Hidden by default (like the "MTP" column it mirrors).
    expect(screen.queryByText(t.tableModelVision)).not.toBeInTheDocument();

    // Enable it via the column-visibility menu.
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.tableModelVision }));

    // rowC is vision-capable → the unified neutral outlined "Vision" chip;
    // rowA/rowB show an en-dash (not the old t.yes / hyphen-minus rendering).
    expect(within(rowFor('GPU-Box-C')).getByText(t.tableModelVision)).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-A')).getByText('–')).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-B')).getByText('–')).toBeInTheDocument();
  });
});
