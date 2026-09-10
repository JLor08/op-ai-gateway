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
  type ModelServerCapability,
  type ModelServerRow,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(() => cleanup());

const model: ModelOption = {
  id: 'qwen-coder',
  display_name: 'qwen-coder',
  flavors: [],
  loading_on_count: 0,
};

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
      // Task 6 (timings-capability-detection): rowA/rowB/rowC each seed a
      // DIFFERENT verdict (supported/unsupported/never-determined) — one
      // fixture per possible value — so a test asserting on one verdict
      // cannot coincidentally pass off another row's value.
      live_progress_support: 'supported',
      live_progress_checked_at: '2026-08-01T08:00:00Z',
      // Capability-table task 5: the four cap_* verdict fields plus
      // capabilities_source/capabilities_checked_at collapsed onto this one
      // rows array -- all-empty baseline for the Capabilities column, like
      // every other verdict field's default here -- individual tests below
      // override this to exercise one determined scenario at a time.
      capabilities: [],
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
      live_progress_support: 'unsupported',
      live_progress_checked_at: '2026-08-01T07:00:00Z',
      capabilities: [],
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
      // Never determined: the checked-at timestamp is also absent, matching
      // what the backend actually produces for "" (see ModelMapping.
      // LiveProgressCheckedAt's doc-comment — nil until a verdict exists).
      live_progress_support: '',
      live_progress_checked_at: null,
      capabilities: [],
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

// cellForColumn resolves a row's cell by its COLUMN HEADER rather than by a
// hard-coded index. Several columns on this table render the identical "—"
// placeholder for their own not-measured case, so an index-based assertion
// silently survives a column being inserted ahead of the one under test — it
// just asserts on the neighbour. The header row is the same table's, so the
// index it yields is the visible-column index the body rows use.
function cellForColumn(serverName: string, columnLabel: string): HTMLElement {
  const row = rowFor(serverName);
  const headers = within(row.closest('table')!).getAllByRole('columnheader');
  const index = headers.findIndex((h) => h.textContent?.trim() === columnLabel);
  if (index < 0) throw new Error(`no column header labelled ${columnLabel}`);
  return within(row).getAllByRole('cell')[index];
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

  // FIX 4 of the final whole-branch review: the column this one replaced
  // treated BOTH `starting` and `pending_vram_unknown` as loading, because the
  // shared runtimeStateBadge maps both onto the `watch` badge ("waiting to be
  // loaded"). Special-casing only `starting` here let the same spec read
  // yellow "Lädt" on the runtime admin screen and grey "Nicht Geladen" on this
  // one — a contradiction of the very vocabulary this column claims to reuse.
  it('treats pending_vram_unknown as Lädt too, matching the shared runtime-state badge', async () => {
    const rows = makeRows().map((r) =>
      r.mapping_id === 'map-c' ? { ...r, state: 'pending_vram_unknown' } : r,
    );
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    expect(within(rowFor('GPU-Box-C')).getByText(t.modelServerLoading)).toHaveAttribute(
      'data-status',
      'watch',
    );
    expect(within(rowFor('GPU-Box-C')).queryByText(t.modelServerNotLoaded)).not.toBeInTheDocument();
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

  it('gates active/queue on their probe state rather than on the number itself', async () => {
    const rows = makeRows().map((r) => {
      // map-a: metrics_probe "ok" stays, but drop active/queue to a genuine 0
      // — the real measured value, not the "never measured" placeholder.
      if (r.mapping_id === 'map-a') return { ...r, active_requests: 0, queue_depth: 0 };
      // map-b: metrics_probe flips to "unreachable" — its NONZERO
      // active_requests/queue_depth were still never measured, so they must
      // render "—", not the raw number.
      if (r.mapping_id === 'map-b') return { ...r, metrics_probe: 'unreachable' };
      return r;
    });
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // Column order (see the previous test): 3 active, 4 queue.
    const cellsA = within(rowFor('GPU-Box-A')).getAllByRole('cell');
    expect(cellsA[3]).toHaveTextContent('0');
    expect(cellsA[4]).toHaveTextContent('0');

    const cellsB = within(rowFor('GPU-Box-B')).getAllByRole('cell');
    expect(cellsB[3]).toHaveTextContent('—');
    expect(cellsB[4]).toHaveTextContent('—');
  });

  // FIX 1 of the final whole-branch review: unlike active/queue, `context_size`
  // is NOT probe-derived. The portal service fills it once from the PERSISTED
  // mapping field (service_model_servers.go: `ContextSize:
  // view.mapping.ContextSize`), which a benchmark run or a manual operator
  // entry sets just as well as the agent's probe. Gating the cell on
  // `context_probe` therefore hid a real, stored context size on every
  // non-probing row. Both REALISTIC shapes of such a row must still show the
  // number:
  //   - ['', ''] — the non-probing row (a non-server_agent model server, or an
  //     agent without the runtime_model_probe capability). Both probe fields
  //     are set together under ONE capability gate (injectRowRuntimeState), so
  //     a row with context_probe "" always has metrics_probe "" too; the
  //     earlier fixture paired "" with a live "ok" metrics probe, which the
  //     backend cannot produce.
  //   - ['ok', 'na'] — a probing agent whose child has no context endpoint
  //     configured at all, while its /metrics endpoint answers fine.
  it.each([
    ['', ''],
    ['ok', 'na'],
  ])(
    'shows a persisted context size when metrics_probe is %j and context_probe is %j',
    async (metricsProbe, contextProbe) => {
      const rows = makeRows().map((r) =>
        r.mapping_id === 'map-c'
          ? {
              ...r,
              // A non-probing row has no runtime state either; keeping the
              // whole row consistent is the point of this fixture.
              state: metricsProbe === '' ? '' : r.state,
              metrics_probe: metricsProbe,
              context_probe: contextProbe,
            }
          : r,
      );
      const { api } = makeApi({
        modelServers: vi.fn().mockResolvedValue(rows),
      } as Partial<ModelServersSectionApi>);
      renderSection(api);
      await screen.findByText('GPU-Box-C');

      // Column order (see above): 8 context. makeRows' rowC persists 8192.
      const cells = within(rowFor('GPU-Box-C')).getAllByRole('cell');
      expect(cells[8]).toHaveTextContent('8192');
    },
  );

  it('renders the context size as "—" only when nothing is stored (0), even with context_probe "ok"', async () => {
    const rows = makeRows().map((r) => (r.mapping_id === 'map-c' ? { ...r, context_size: 0 } : r));
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // A real context size of 0 cannot happen, so 0 means "nothing known" —
    // the one case the "—" placeholder is for.
    const cells = within(rowFor('GPU-Box-C')).getAllByRole('cell');
    expect(cells[8]).toHaveTextContent('—');
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

  // Task 6 (timings-capability-detection): the PERSISTED live-progress-support
  // verdict, beside the Kontext column. makeRows() seeds a DIFFERENT verdict
  // per row (rowA "supported", rowB "unsupported", rowC "" never-determined)
  // specifically so this test's assertions cannot pass by coincidence off the
  // wrong row's fixture value.
  it('renders the live-progress verdict as a badge BY KEY, and the shared em-dash when never determined', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowA: "supported" -> the positive badge. Asserted by KEY (data-status),
    // never colour: this portal has no red, and shared/status.ts collapses
    // several distinct statuses onto the same visual appearance, so a colour
    // assertion here would go stale silently.
    expect(
      within(rowFor('GPU-Box-A')).getByText(t.modelServerLiveProgressSupported),
    ).toHaveAttribute('data-status', 'active');

    // rowB: "unsupported" -> the NEUTRAL badge (standby), deliberately NOT the
    // attention/"watch" badge every other explicit-state column on this
    // screen reaches for: an older llama.cpp build that lacks this request
    // parameter is not broken, it simply lacks a nicety, and flagging it
    // would put a warning on every such server.
    expect(
      within(rowFor('GPU-Box-B')).getByText(t.modelServerLiveProgressUnsupported),
    ).toHaveAttribute('data-status', 'standby');

    // rowC: "" (never determined) -> the design spec calls for rendering
    // NOTHING here; that is deliberately overridden to the shared "—"
    // placeholder instead, so the column reads as "present, nothing
    // determined yet" rather than as a column that doesn't exist. No
    // badge/chip is rendered for it either.
    //
    // Scoped to this column BY HEADER, not by index: the immediately
    // preceding Kontext column renders the identical glyph for its own
    // not-measured case, so an index-based query would stay green while
    // asserting on the wrong cell if a column were ever inserted ahead of
    // this one.
    expect(cellForColumn('GPU-Box-C', t.modelServerColLiveProgress)).toHaveTextContent('—');
    expect(
      within(rowFor('GPU-Box-C')).queryByText(t.modelServerLiveProgressSupported),
    ).not.toBeInTheDocument();
    expect(
      within(rowFor('GPU-Box-C')).queryByText(t.modelServerLiveProgressUnsupported),
    ).not.toBeInTheDocument();
  });

  it('opens the "supported" live-progress tooltip on hover, naming the verdict and the checked-at time', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    fireEvent.mouseOver(screen.getByText(t.modelServerLiveProgressSupported));
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).toHaveTextContent(t.modelServerLiveProgressTooltipSupported);
    // rowA's live_progress_checked_at is '2026-08-01T08:00:00Z'.
    expect(tooltip).toHaveTextContent(
      t.modelServerLiveProgressCheckedAt(new Date('2026-08-01T08:00:00Z').toLocaleString()),
    );
  });

  it('opens the "unsupported" live-progress tooltip on hover, naming the reason and the checked-at time', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    fireEvent.mouseOver(screen.getByText(t.modelServerLiveProgressUnsupported));
    const tooltip = await screen.findByRole('tooltip');
    // Names the reason: the live figure is unavailable because this build's
    // request schema has no such field.
    expect(tooltip).toHaveTextContent(t.modelServerLiveProgressTooltipUnsupported);
    // rowB's live_progress_checked_at is '2026-08-01T07:00:00Z'.
    expect(tooltip).toHaveTextContent(
      t.modelServerLiveProgressCheckedAt(new Date('2026-08-01T07:00:00Z').toLocaleString()),
    );
  });

  // capRow builds one ModelServerCapability fixture entry -- capability-table
  // task 5's rows array replaced the four dedicated cap_* verdict fields, so
  // every test below builds the array explicitly instead of setting a field.
  function capRow(
    capability: string,
    verdict: string,
    source = 'llama_cpp_props',
    checkedAt = '2026-08-01T06:00:00Z',
  ): ModelServerCapability {
    return { capability, verdict, source, checked_at: checkedAt };
  }

  // Capability-table task 5: the Capabilities column, beside Live-Fortschritt.
  // Scoped BY HEADER via cellForColumn throughout, not by cell position or a
  // bare getByText: the neighbouring Live-Fortschritt column renders the
  // identical "—" placeholder for its own not-determined case, so a
  // positional assertion would keep passing even against the wrong cell if a
  // column were ever inserted ahead of this one.
  it('renders exactly one chip per determined `yes` verdict, and no chip at all for a `no` or a missing row', async () => {
    const rows = makeRows().map((r) =>
      r.mapping_id === 'map-a'
        ? {
            ...r,
            capabilities: [
              capRow('vision', 'yes'),
              capRow('video', 'no'),
              capRow('tools', 'yes'),
              // 'audio' has NO row at all here -- never determined, same as a
              // "no" for chip-rendering purposes, so no chip either.
            ],
          }
        : r,
    );
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
    // Exactly two chips (Vision, Tools) -- audio has no row (never
    // determined, no chip) and video is "no" (a negative, also no chip).
    expect(within(cell).getByText(t.capabilityVision)).toBeInTheDocument();
    expect(within(cell).getByText(t.capabilityTools)).toBeInTheDocument();
    expect(within(cell).queryByText(t.capabilityVideo)).not.toBeInTheDocument();
    expect(within(cell).queryByText(t.capabilityAudio)).not.toBeInTheDocument();
    // Both rendered chips are keyed by data-status, never by colour (house
    // rule; this portal has no red) -- both are the same neutral "success"
    // key since there is no ranking among determined capabilities.
    expect(within(cell).getByText(t.capabilityVision)).toHaveAttribute('data-status', 'active');
    expect(within(cell).getByText(t.capabilityTools)).toHaveAttribute('data-status', 'active');
  });

  // Task 6: "speculation_observed" got a translated label and a fixed slot
  // in KNOWN_CAPABILITY_ORDER (after Tools -- see that array's own comment),
  // instead of the verbatim "unrecognized name" chip it rendered before this
  // task touched the frontend at all.
  function makeToolsAndSpeculationRows(): ModelServerRow[] {
    return makeRows().map((r) =>
      r.mapping_id === 'map-a'
        ? {
            ...r,
            capabilities: [
              capRow('tools', 'yes'),
              capRow('speculation_observed', 'yes', 'llama_cpp_timings', '2026-09-01T06:00:00Z'),
            ],
          }
        : r,
    );
  }

  it('renders speculation_observed as a translated chip after Tools', async () => {
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(makeToolsAndSpeculationRows()),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
    // Translated label, not the raw wire string.
    expect(within(cell).getByText(t.capabilitySpeculationObserved)).toBeInTheDocument();
    expect(within(cell).queryByText('speculation_observed')).not.toBeInTheDocument();
    // Known-vocabulary chip: the vetted "active" key, same as any other
    // fixed-order verdict chip -- not the neutral "standby" the unrecognized-
    // name bucket uses.
    expect(within(cell).getByText(t.capabilitySpeculationObserved)).toHaveAttribute(
      'data-status',
      'active',
    );
    // Fixed order: Tools precedes Speculation observed.
    const labels = within(cell)
      .getAllByText((_, el) => el?.getAttribute('data-status') === 'active')
      .map((el) => el.textContent);
    expect(labels).toEqual([t.capabilityTools, t.capabilitySpeculationObserved]);
  });

  // Two separate `it`s, each its own render + single hover -- following this
  // file's own established pattern (see makeTwoCapabilityRows below) rather
  // than hovering twice in one test: MUI keeps the first Popper mounted
  // (display:none, still in the DOM) once a second tooltip opens, so
  // `findByRole('tooltip')` after a second hover can resolve the stale node.
  it('folds the speculation-observed caveat into its OWN chip’s tooltip, beside the shared caveat and provenance', async () => {
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(makeToolsAndSpeculationRows()),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
    fireEvent.mouseOver(within(cell).getByText(t.capabilitySpeculationObserved));
    const tooltip = await screen.findByRole('tooltip');
    // The capability-specific "observed, not supported" caveat is ADDED to,
    // not swapped for, the shared video/tools text and this row's own
    // source/checked-at.
    expect(tooltip).toHaveTextContent(t.modelServerCapabilitiesTooltip);
    expect(tooltip).toHaveTextContent(t.capabilitySpeculationObservedTooltip);
    expect(tooltip).toHaveTextContent(t.modelServerCapabilitiesSource('llama_cpp_timings'));
    expect(tooltip).toHaveTextContent(
      t.modelServerCapabilitiesCheckedAt(new Date('2026-09-01T06:00:00Z').toLocaleString()),
    );
  });

  it('does NOT leak the speculation-observed caveat into a different chip’s tooltip on the same row', async () => {
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(makeToolsAndSpeculationRows()),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
    fireEvent.mouseOver(within(cell).getByText(t.capabilityTools));
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).not.toHaveTextContent(t.capabilitySpeculationObservedTooltip);
  });

  it('renders the shared em-dash when the capabilities array is empty', async () => {
    const { api } = makeApi();
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    // rowB and rowC both carry an empty capabilities array in makeRows().
    expect(cellForColumn('GPU-Box-B', t.modelServerColCapabilities)).toHaveTextContent('—');
    expect(cellForColumn('GPU-Box-C', t.modelServerColCapabilities)).toHaveTextContent('—');
  });

  it('renders an unrecognized capability name as a chip labelled VERBATIM, after the fixed-order verdict chips', async () => {
    const rows = makeRows().map((r) =>
      r.mapping_id === 'map-b'
        ? { ...r, capabilities: [capRow('audio', 'yes'), capRow('thinking', 'yes')] }
        : r,
    );
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    const cell = cellForColumn('GPU-Box-B', t.modelServerColCapabilities);
    // The unknown "thinking" string renders verbatim, not dropped -- the
    // capability vocabulary is open-ended upstream, so an unrecognized entry
    // must still surface rather than silently disappear.
    expect(within(cell).getByText('thinking')).toBeInTheDocument();
    expect(within(cell).getByText(t.capabilityAudio)).toBeInTheDocument();
    // The verdict chip is the vetted "active" key; the unrecognized-name chip
    // is the neutral "standby" key -- an unrecognized, un-vetted string must
    // not be equated with a caveated verdict this codebase actually vouches
    // for.
    expect(within(cell).getByText(t.capabilityAudio)).toHaveAttribute('data-status', 'active');
    expect(within(cell).getByText('thinking')).toHaveAttribute('data-status', 'standby');
    // Fixed order: the Audio verdict chip precedes the unrecognized-name
    // chip. Scoped to BOTH keys (not just 'active') so the standby chip is
    // still picked up; if the push order were ever scrambled this would
    // still catch it, since the two keys involved are distinct.
    const labels = within(cell)
      .getAllByText((_, el) =>
        ['active', 'standby'].includes(el?.getAttribute('data-status') ?? ''),
      )
      .map((el) => el.textContent);
    expect(labels).toEqual([t.capabilityAudio, 'thinking']);
  });

  // "mtp"/"live_progress" are rows in this SAME capabilities array now (they
  // share the model_mapping_capabilities table with vision/video/audio/
  // tools), but this table already has its OWN dedicated "MTP"/
  // "Live-Fortschritt" columns for them -- without this exclusion they would
  // ALSO fall into the verbatim unknown-name bucket and duplicate those
  // columns as a raw, untranslated chip.
  it('does not render a duplicate chip for mtp or live_progress -- they have their own columns', async () => {
    const rows = makeRows().map((r) =>
      r.mapping_id === 'map-a'
        ? { ...r, capabilities: [capRow('mtp', 'yes'), capRow('live_progress', 'yes')] }
        : r,
    );
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
    expect(within(cell).queryByText('mtp')).not.toBeInTheDocument();
    expect(within(cell).queryByText('live_progress')).not.toBeInTheDocument();
    // Neither is a known label either -- nothing else was seeded, so the
    // column falls back to the shared em-dash.
    expect(cell).toHaveTextContent('—');
  });

  // An upstream's capability vocabulary is OPEN, so nothing stops it
  // reporting a name that happens to equal one of this build's own translated
  // labels. Keyed by the LABEL, the verbatim "Vision" chip and the "vision"
  // row's translated "Vision" chip collide, which React reports as a
  // duplicate-key error and reconciles wrongly. Keyed by the capability name
  // they cannot: two capabilities, two chips, no warning.
  it('renders both chips when an upstream capability name equals a translated label', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      const rows = makeRows().map((r) =>
        r.mapping_id === 'map-a'
          ? { ...r, capabilities: [capRow('vision', 'yes'), capRow('Vision', 'yes')] }
          : r,
      );
      const { api } = makeApi({
        modelServers: vi.fn().mockResolvedValue(rows),
      } as Partial<ModelServersSectionApi>);
      renderSection(api);
      await screen.findByText('GPU-Box-C');

      const cell = cellForColumn('GPU-Box-A', t.modelServerColCapabilities);
      expect(within(cell).getAllByText(t.capabilityVision)).toHaveLength(2);
      const duplicateKeyWarnings = consoleError.mock.calls
        .map((args) => args.join(' '))
        .filter((message) => message.includes('same key'));
      expect(duplicateKeyWarnings).toEqual([]);
    } finally {
      consoleError.mockRestore();
    }
  });

  // A capability row carries `checked_at` as a Go time.Time with no
  // `omitempty`, so a verdict that was never timestamped arrives as
  // "0001-01-01T00:00:00Z" -- rendered unguarded, the tooltip claims the
  // capability was determined in the year 1. Each part of the tooltip is
  // guarded independently, exactly as the shared row-wide tooltip this
  // replaced guarded its own two.
  it('omits the source and checked-at clauses when the row carries neither', async () => {
    const rows = makeRows().map((r) =>
      r.mapping_id === 'map-a'
        ? { ...r, capabilities: [capRow('vision', 'yes', '', '0001-01-01T00:00:00Z')] }
        : r,
    );
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(rows),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    fireEvent.mouseOver(within(rowFor('GPU-Box-A')).getByText(t.capabilityVision));
    const tooltip = await screen.findByRole('tooltip');
    // The shared caveat still shows -- it is the part that always applies.
    expect(tooltip).toHaveTextContent(t.modelServerCapabilitiesTooltip);
    expect(tooltip.textContent ?? '').not.toMatch(/0*1\.|0001/);
    expect(tooltip).not.toHaveTextContent(t.modelServerCapabilitiesSource(''));
  });

  // These two capabilities on the SAME row carry DIFFERENT source/checked-at
  // values, so a test asserting on one chip's tooltip cannot coincidentally
  // pass off the other's -- this is the whole point of the per-capability
  // tooltip (the shared one this replaced could only ever show ONE
  // source/date for the entire row). Two separate `it`s (rather than hovering
  // one chip then the other in the same test) since a real user interaction
  // is needed to dismiss the first tooltip before a second can be asserted on
  // in isolation, and each `it` already gets its own fresh render.
  function makeTwoCapabilityRows(): ModelServerRow[] {
    return makeRows().map((r) =>
      r.mapping_id === 'map-a'
        ? {
            ...r,
            capabilities: [
              capRow('vision', 'yes', 'llama_cpp_props', '2026-08-01T06:00:00Z'),
              capRow('tools', 'yes', 'legacy', '2026-07-01T00:00:00Z'),
            ],
          }
        : r,
    );
  }

  it('opens the capabilities tooltip on hover, naming that capability’s own provenance and checked-at time', async () => {
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(makeTwoCapabilityRows()),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    fireEvent.mouseOver(within(rowFor('GPU-Box-A')).getByText(t.capabilityVision));
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).toHaveTextContent(t.modelServerCapabilitiesSource('llama_cpp_props'));
    expect(tooltip).toHaveTextContent(
      t.modelServerCapabilitiesCheckedAt(new Date('2026-08-01T06:00:00Z').toLocaleString()),
    );
    // The shared upstream caveats (video = build + vision encoder, tools =
    // native-template quality, not availability) are always folded in too.
    expect(tooltip).toHaveTextContent(t.modelServerCapabilitiesTooltip);
    // The OTHER capability's source on this same row must not leak in.
    expect(tooltip).not.toHaveTextContent(t.modelServerCapabilitiesSource('legacy'));
  });

  it('names a DIFFERENT capability’s own provenance in its own tooltip, on the same row', async () => {
    const { api } = makeApi({
      modelServers: vi.fn().mockResolvedValue(makeTwoCapabilityRows()),
    } as Partial<ModelServersSectionApi>);
    renderSection(api);
    await screen.findByText('GPU-Box-C');

    fireEvent.mouseOver(within(rowFor('GPU-Box-A')).getByText(t.capabilityTools));
    const tooltip = await screen.findByRole('tooltip');
    expect(tooltip).toHaveTextContent(t.modelServerCapabilitiesSource('legacy'));
    expect(tooltip).toHaveTextContent(
      t.modelServerCapabilitiesCheckedAt(new Date('2026-07-01T00:00:00Z').toLocaleString()),
    );
    expect(tooltip).not.toHaveTextContent(t.modelServerCapabilitiesSource('llama_cpp_props'));
  });
});
