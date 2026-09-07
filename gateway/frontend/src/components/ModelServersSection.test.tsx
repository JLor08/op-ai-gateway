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
      state: 'running',
      metrics_probe: 'ok',
      context_probe: 'ok',
      active_requests: 5,
      queue_depth: 3,
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
      state: '',
      metrics_probe: 'ok',
      context_probe: 'ok',
      active_requests: 0,
      queue_depth: 0,
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
      state: 'starting',
      metrics_probe: 'ok',
      context_probe: 'ok',
      active_requests: 0,
      queue_depth: 3,
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

  it('reflects loaded-state and updates it live on an SSE frame (state:"" fallback)', async () => {
    const { api, getOnData } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowA (state 'running') → Geladen. rowB (state '', loaded:false) → the
    // `loaded`-boolean fallback → Nicht Geladen.
    expect(within(rowFor('GPU-Box-A')).getByText(t.tableModelLoaded)).toBeInTheDocument();
    expect(within(rowFor('GPU-Box-B')).getByText(t.modelServerNotLoaded)).toBeInTheDocument();

    // A live update flips rowB's `loaded` flag. rowB's `state` stays "", so the
    // merged Status column follows the fallback boolean exactly like the old
    // separate "Geladen" column did.
    const updated = makeRows().map((r) => (r.mapping_id === 'map-b' ? { ...r, loaded: true } : r));
    act(() => getOnData()!(updated));
    expect(within(rowFor('GPU-Box-B')).getByText(t.tableModelLoaded)).toBeInTheDocument();
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

  it('shows Geladen for a running row and Lädt for a starting row, reusing the runtime-state colour vocabulary', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowC is 'starting' → "Lädt", the same "currently loading" (yellow/watch)
    // colour RuntimeAdminSection's Live-status column gives that state (shared
    // via components/shared/runtimeState.ts's runtimeStateBadge). Also assert
    // the chip's data-status KEY (StatusChip.tsx's documented
    // getByText(label).toHaveAttribute('data-status', key) pattern, mirrored
    // from RuntimeAdminSection.test.tsx) so a dropped runtimeStateBadge(r.state)
    // call — which would flatten every chip to one fixed grey key — fails here,
    // not just a missing label.
    expect(within(rowFor('GPU-Box-C')).getByText(t.modelServerLoading)).toHaveAttribute(
      'data-status',
      'watch',
    );
    // rowA is 'running' → "Geladen" with the active (green) badge.
    expect(within(rowFor('GPU-Box-A')).getByText(t.tableModelLoaded)).toHaveAttribute(
      'data-status',
      'active',
    );
  });

  it('falls back to the `loaded` boolean when state is "", and collapses any other explicit runtime state to Nicht Geladen', async () => {
    const rows = makeRows().map((r) => {
      // map-a: an explicit runtime state this column does NOT special-case
      // (neither "running" nor "starting") — the "otherwise" branch.
      if (r.mapping_id === 'map-a') return { ...r, state: 'crashed' };
      // map-b: state stays "" (no agent-managed runtime status at all) but
      // `loaded` flips true — the `loaded`-boolean fallback's OTHER half (the
      // "" + loaded:false case is already covered by rowB's own default state
      // in the "reflects loaded-state" test above).
      if (r.mapping_id === 'map-b') return { ...r, loaded: true };
      return r;
    });
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowA: state "crashed" → neither running nor starting → "Nicht Geladen",
    // standby (grey) — the same "otherwise" bucket every non-running/starting
    // runtime state falls into, not a fourth chip colour.
    expect(within(rowFor('GPU-Box-A')).getByText(t.modelServerNotLoaded)).toHaveAttribute(
      'data-status',
      'standby',
    );
    // rowB: state "" + loaded:true → the fallback → "Geladen", active (green).
    expect(within(rowFor('GPU-Box-B')).getByText(t.tableModelLoaded)).toHaveAttribute(
      'data-status',
      'active',
    );

    // The old separate "Geladen"/"Live-Status" columns, and the raw
    // runtime-state vocabulary (Läuft/Startet…/Unbekannt) the removed
    // Live-Status column used to render, are gone — the merged column only
    // ever shows Geladen/Lädt/Nicht Geladen.
    expect(screen.queryByText(t.runtimeLiveStatus)).not.toBeInTheDocument();
    expect(screen.queryByText(t.runtimeStateRunning)).not.toBeInTheDocument();
    expect(screen.queryByText(t.runtimeStateStarting)).not.toBeInTheDocument();
    expect(screen.queryByText(t.runtimeStatusUnknown)).not.toBeInTheDocument();
  });

  it('shows the live active/queue counts and the context size', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-A');

    // Pin each value to its OWN column by cell position rather than asserting
    // getByText independently for each — a swap of the active/queue column
    // `render` functions would still leave both numbers present somewhere in
    // the row. Indices follow the default-visible `columns` order declared in
    // ModelServersSection.tsx (defaultHidden columns excluded), post-merge:
    // 0 prio, 1 server, 2 status, 3 active, 4 queue, 5 genTps, 6 promptTps,
    // 7 loadTime, 8 context.
    const cells = within(rowFor('GPU-Box-A')).getAllByRole('cell');
    // active_requests=5, queue_depth=3, context_size=32768 (makeRows' rowA,
    // metrics_probe/context_probe both "ok").
    expect(cells[3]).toHaveTextContent('5');
    expect(cells[4]).toHaveTextContent('3');
    expect(cells[8]).toHaveTextContent('32768');
  });

  it('renders 0 active/queue as the real value, not a placeholder dash, when metrics_probe is "ok"', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-B');

    // rowB: metrics_probe "ok", active_requests=0, queue_depth=0 — a genuinely
    // idle, actually-measured server, distinct from the "—" placeholder a
    // probe state other than "ok" renders instead.
    const cells = within(rowFor('GPU-Box-B')).getAllByRole('cell');
    expect(cells[3]).toHaveTextContent('0');
    expect(cells[4]).toHaveTextContent('0');
  });

  it('gates active/queue/context on their probe state rather than on the number itself', async () => {
    const rows = makeRows().map((r) => {
      // map-a: metrics_probe "ok" stays, but drop active/queue to a genuine 0
      // — the real measured value, not the "never measured" placeholder.
      if (r.mapping_id === 'map-a') return { ...r, active_requests: 0, queue_depth: 0 };
      // map-b: metrics_probe flips to "unreachable" — its NONZERO
      // active_requests/queue_depth were still never measured, so they must
      // render "—", not the raw number.
      if (r.mapping_id === 'map-b') return { ...r, metrics_probe: 'unreachable' };
      // map-c: context_probe flips to "na" — its nonzero context_size was
      // never measured (this runtime type has no context endpoint) and must
      // render "—" too.
      if (r.mapping_id === 'map-c') return { ...r, context_probe: 'na' };
      return r;
    });
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // Column order (see the previous test): 3 active, 4 queue, 8 context.
    const cellsA = within(rowFor('GPU-Box-A')).getAllByRole('cell');
    expect(cellsA[3]).toHaveTextContent('0');
    expect(cellsA[4]).toHaveTextContent('0');

    const cellsB = within(rowFor('GPU-Box-B')).getAllByRole('cell');
    expect(cellsB[3]).toHaveTextContent('—');
    expect(cellsB[4]).toHaveTextContent('—');

    const cellsC = within(rowFor('GPU-Box-C')).getAllByRole('cell');
    expect(cellsC[8]).toHaveTextContent('—');
  });

  it.each(['unreachable', 'na', ''])(
    'treats metrics_probe %j the same way: active/queue render "—", never the raw number',
    async (probe) => {
      const rows = makeRows().map((r) =>
        r.mapping_id === 'map-c'
          ? { ...r, metrics_probe: probe, active_requests: 9, queue_depth: 6 }
          : r,
      );
      const { api } = makeApi({
        modelServers: vi.fn().mockResolvedValue(rows),
      } as Partial<ModelServersSectionApi>);
      renderSection(api);
      await screen.findByText('GPU-Box-C');

      const cells = within(rowFor('GPU-Box-C')).getAllByRole('cell');
      expect(cells[3]).toHaveTextContent('—');
      expect(cells[4]).toHaveTextContent('—');
    },
  );
});
