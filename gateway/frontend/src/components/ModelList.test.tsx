// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, within, fireEvent, waitFor } from '@testing-library/react';
import { ModelList } from './ModelList';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { ModelOption } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(() => cleanup());

// vision: true on both so the vision column's own dash never collides with the
// dash under test in the loaded/offered columns below (each row keeps exactly
// one "-" — its own).
const models: ModelOption[] = [
  {
    id: 'qwen-coder',
    display_name: 'qwen-coder',
    flavors: ['openai'],
    loaded: true,
    loaded_on: ['GPU-Box'],
    offered_on_count: 3,
    vision: true,
  },
  { id: 'llama3', display_name: 'llama3', flavors: ['openai'], offered_on_count: 2, vision: true },
];

// ModelList now calls useToast (for optimistic visibility errors), so every render
// must sit inside a ToastProvider.
function renderList(props: Parameters<typeof ModelList>[0]) {
  return render(
    <ToastProvider>
      <ModelList {...props} />
    </ToastProvider>,
  );
}

describe('ModelList loaded column', () => {
  it('renders a Loaded column header', () => {
    renderList({ t, models });
    expect(screen.getByText(t.tableModelLoaded)).toBeInTheDocument();
    // The page subtitle is the dedicated modelsIntro (an explicit user request), not
    // the shared stub placeholder — pin it so a future edit can't silently revert it.
    expect(screen.getByText(t.modelsIntro)).toBeInTheDocument();
  });

  it('shows the loaded server COUNT for a loaded model and a dash for an unloaded one', () => {
    renderList({ t, models });
    // The loaded column now shows a COUNT of servers (1), not the server names.
    const loadedRow = screen.getAllByText('qwen-coder')[0].closest('tr')!;
    expect(within(loadedRow).getByText('1')).toBeInTheDocument();
    // The server name is no longer rendered anywhere in the list.
    expect(screen.queryByText('GPU-Box')).toBeNull();

    // The unloaded row shows "-" in its loaded cell (flavors is "openai", so the
    // only dash in the row is the loaded cell).
    const unloadedRow = screen.getAllByText('llama3')[0].closest('tr')!;
    expect(within(unloadedRow).getByText('-')).toBeInTheDocument();
  });
});

describe('ModelList vision column', () => {
  it('renders a neutral outlined chip for a vision-capable model and an en-dash for a non-capable one', () => {
    // offered_on_count + loaded set on both so the offered/loaded cells never
    // render a dash themselves — the row's only dash is the vision cell under test.
    const local: ModelOption[] = [
      {
        id: 'vision-model',
        display_name: 'vision-model',
        flavors: ['openai'],
        vision: true,
        offered_on_count: 1,
        loaded: true,
        loaded_on: ['srv-a'],
      },
      {
        id: 'text-model',
        display_name: 'text-model',
        flavors: ['openai'],
        vision: false,
        offered_on_count: 1,
        loaded: true,
        loaded_on: ['srv-a'],
      },
    ];
    renderList({ t, models: local });
    // The column header renders (its text also appears in the vision-row chip
    // below, so use getAllByText here rather than the single-match getByText).
    expect(screen.getAllByText(t.tableModelVision).length).toBeGreaterThan(0);

    const visionRow = screen.getAllByText('vision-model')[0].closest('tr')!;
    const visionChip = within(visionRow).getByText(t.tableModelVision);
    expect(visionChip).toBeInTheDocument();
    // Unified rendering: a neutral MUI Chip with variant="outlined" and no color
    // (i.e. not the old color="success" chip).
    const chipRoot = visionChip.closest('.MuiChip-root')!;
    expect(chipRoot).toHaveClass('MuiChip-outlined');
    expect(chipRoot.className).not.toMatch(/MuiChip-colorSuccess/);

    const textRow = screen.getAllByText('text-model')[0].closest('tr')!;
    // en dash (U+2013), not a hyphen-minus.
    expect(within(textRow).getByText('–')).toBeInTheDocument();
  });
});

describe('ModelList loading vs empty', () => {
  it('shows the loading label while loading with no models', () => {
    renderList({ t, models: [], loading: true });
    expect(screen.getByText(t.loading)).toBeInTheDocument();
    expect(screen.queryByText(t.modelsEmpty)).toBeNull();
  });

  it('shows the empty label (not loading) when loaded with no models', () => {
    renderList({ t, models: [], loading: false });
    expect(screen.getByText(t.modelsEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).toBeNull();
  });
});

describe('ModelList offered column', () => {
  it('renders an Offered column header BEFORE the Loaded header', () => {
    renderList({ t, models });
    expect(screen.getByText(t.tableModelOffered)).toBeInTheDocument();
    // The "Angeboten" column sits immediately before "Geladen": compare their
    // positions in the ordered column-header list.
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent ?? '');
    const offeredIdx = headers.findIndex((txt) => txt.includes(t.tableModelOffered));
    const loadedIdx = headers.findIndex((txt) => txt.includes(t.tableModelLoaded));
    expect(offeredIdx).toBeGreaterThanOrEqual(0);
    expect(loadedIdx).toBeGreaterThanOrEqual(0);
    expect(offeredIdx).toBeLessThan(loadedIdx);
  });

  it('shows the offered-server COUNT for a model and a dash when it has none', () => {
    const local: ModelOption[] = [
      {
        id: 'with-offered',
        display_name: 'with-offered',
        flavors: ['openai'],
        offered_on_count: 3,
        vision: true,
      },
      // No offered_on_count → the offered cell shows "-". It IS loaded (count 1) and
      // vision-capable, so neither of those cells is a dash, leaving exactly one
      // "-" in the row (offered).
      {
        id: 'no-offered',
        display_name: 'no-offered',
        flavors: ['openai'],
        loaded: true,
        loaded_on: ['srv-a'],
        vision: true,
      },
    ];
    renderList({ t, models: local });

    const offeredRow = screen.getAllByText('with-offered')[0].closest('tr')!;
    expect(within(offeredRow).getByText('3')).toBeInTheDocument();

    const noneRow = screen.getAllByText('no-offered')[0].closest('tr')!;
    expect(within(noneRow).getByText('-')).toBeInTheDocument();
  });
});

describe('ModelList details sub-view', () => {
  it('opens the per-model detail sub-view via the Details row action', async () => {
    const modelServers = vi.fn(async () => []);
    const subscribeModelServers = vi.fn(() => () => {});
    const api = { modelServers, subscribeModelServers } as unknown as PortalApi;
    renderList({ t, models, api });

    // Each row has a single "Details" action → rendered inline as a labelled icon
    // button. The first row is the (declaration-order) loaded model qwen-coder.
    fireEvent.click(screen.getAllByRole('button', { name: t.modelDetailsAction })[0]);

    // The detail sub-view mounted: the ModelServersSection panel title + the
    // breadcrumb appear, and it fetched + subscribed for the chosen model.
    expect(await screen.findByText(t.modelServerTitle)).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: t.breadcrumb })).toBeInTheDocument();
    expect(modelServers).toHaveBeenCalledWith('qwen-coder');
    expect(subscribeModelServers).toHaveBeenCalledWith('qwen-coder', expect.any(Function));
  });

  it('reports the detail sub-view open/closed state via onDetailViewChange', async () => {
    const modelServers = vi.fn(async () => []);
    const subscribeModelServers = vi.fn(() => () => {});
    const api = { modelServers, subscribeModelServers } as unknown as PortalApi;
    const onDetail = vi.fn();
    renderList({ t, models, api, onDetailViewChange: onDetail });

    // Open the per-model detail sub-view → the parent is told it is open (true).
    fireEvent.click(screen.getAllByRole('button', { name: t.modelDetailsAction })[0]);
    expect(await screen.findByText(t.modelServerTitle)).toBeInTheDocument();
    expect(onDetail).toHaveBeenLastCalledWith(true);

    // Navigate back to the list via the breadcrumb "Back" button → closed (false).
    fireEvent.click(screen.getByRole('button', { name: t.back }));
    await waitFor(() => expect(onDetail).toHaveBeenLastCalledWith(false));
  });

  it('opens the GROUP-detail view (not the per-model one) for a group row', async () => {
    const withGroup: ModelOption[] = [
      { id: 'fast-group', display_name: 'fast-group', flavors: [], is_group: true },
      ...models,
    ];
    const modelServers = vi.fn(async () => []);
    const subscribeModelServers = vi.fn(() => () => {});
    const modelGroupServers = vi.fn(async () => []);
    const api = { modelServers, subscribeModelServers, modelGroupServers } as unknown as PortalApi;
    renderList({ t, models: withGroup, api });

    // The group row is first (declaration order); its "Details" action opens the
    // group view, which fetches by group id via modelGroupServers — NOT the
    // per-model modelServers/subscribeModelServers pair.
    fireEvent.click(screen.getAllByRole('button', { name: t.modelDetailsAction })[0]);

    expect(await screen.findByText(t.modelServerColModel)).toBeInTheDocument();
    expect(screen.getByText(t.groupServersIntro)).toBeInTheDocument();
    expect(modelGroupServers).toHaveBeenCalledWith('fast-group');
    expect(modelServers).not.toHaveBeenCalled();
    expect(subscribeModelServers).not.toHaveBeenCalled();
  });

  it('still opens the per-model detail view for a non-group row', async () => {
    const modelServers = vi.fn(async () => []);
    const subscribeModelServers = vi.fn(() => () => {});
    const modelGroupServers = vi.fn(async () => []);
    const api = { modelServers, subscribeModelServers, modelGroupServers } as unknown as PortalApi;
    renderList({ t, models, api });

    fireEvent.click(screen.getAllByRole('button', { name: t.modelDetailsAction })[0]);

    // ModelServersSection has no "Modell" column (it is already scoped to one
    // model) and no group-detail subtitle — the opposite of the group view.
    expect(await screen.findByText(t.modelServerTitle)).toBeInTheDocument();
    expect(screen.queryByText(t.modelServerColModel)).toBeNull();
    expect(screen.queryByText(t.groupServersIntro)).toBeNull();
    expect(modelServers).toHaveBeenCalledWith('qwen-coder');
    expect(subscribeModelServers).toHaveBeenCalledWith('qwen-coder', expect.any(Function));
    expect(modelGroupServers).not.toHaveBeenCalled();
  });
});

describe('ModelList visibility', () => {
  it('non-admins see a read-only visibility chip (no select)', () => {
    renderList({ t, models });
    // No editable visibility combobox is rendered for a non-admin.
    expect(screen.queryByRole('combobox', { name: `${t.modelVisibility}: llama3` })).toBeNull();
    // The read-only chip shows the default "shown" label.
    expect(screen.getAllByText(t.modelVisibilityShown).length).toBeGreaterThan(0);
  });

  it("admins change a model's visibility via the select and refresh the list", async () => {
    const setModelVisibility = vi.fn(async () => ({ ok: true }));
    const onModelsChanged = vi.fn();
    const api = { setModelVisibility } as unknown as PortalApi;
    renderList({ t, models, api, isAdmin: true, onModelsChanged });

    fireEvent.mouseDown(screen.getByRole('combobox', { name: `${t.modelVisibility}: llama3` }));
    fireEvent.click(await screen.findByRole('option', { name: t.modelVisibilityHidden }));

    await waitFor(() => expect(setModelVisibility).toHaveBeenCalledWith('llama3', 'hidden'));
    expect(onModelsChanged).toHaveBeenCalled();
  });

  it("admins get an editable select preset to 'hidden' for a hidden model (revertible)", async () => {
    // The admin management surface feeds ModelList the UNSUPPRESSED list, so a
    // hidden model appears with an editable select pre-selected to Hidden and can
    // be reverted back to Shown.
    const hiddenModels: ModelOption[] = [
      { id: 'trapped', display_name: 'trapped', flavors: ['openai'], visibility: 'hidden' },
    ];
    const setModelVisibility = vi.fn(async () => ({ ok: true }));
    const onModelsChanged = vi.fn();
    const api = { setModelVisibility } as unknown as PortalApi;
    renderList({ t, models: hiddenModels, api, isAdmin: true, onModelsChanged });

    const combo = screen.getByRole('combobox', { name: `${t.modelVisibility}: trapped` });
    // The row is present and its select reflects the hidden state ("trapped"
    // appears in both the id and display_name cells).
    expect(within(combo.closest('tr')!).getAllByText('trapped').length).toBeGreaterThan(0);
    expect(combo).toHaveTextContent(t.modelVisibilityHidden);

    // Revert it back to Shown.
    fireEvent.mouseDown(combo);
    fireEvent.click(await screen.findByRole('option', { name: t.modelVisibilityShown }));
    await waitFor(() => expect(setModelVisibility).toHaveBeenCalledWith('trapped', 'shown'));
    expect(onModelsChanged).toHaveBeenCalled();
  });

  it("admins edit a GROUP row's visibility via its select (group chip stays)", async () => {
    const withGroup: ModelOption[] = [
      {
        id: 'fast',
        display_name: 'fast',
        flavors: ['openai'],
        is_group: true,
        visibility: 'hidden',
      },
      { id: 'llama3', display_name: 'llama3', flavors: ['openai'] },
    ];
    const setModelVisibility = vi.fn(async () => ({ ok: true }));
    const onModelsChanged = vi.fn();
    const api = { setModelVisibility } as unknown as PortalApi;
    renderList({ t, models: withGroup, api, isAdmin: true, onModelsChanged });

    const groupRow = screen.getAllByText('fast')[0].closest('tr')!;
    // The group indicator now lives in its own "Typ" column (still within this row).
    expect(within(groupRow).getByText(t.modelGroupChip)).toBeInTheDocument();
    // The group row now has an editable visibility select, preset to its state.
    const combo = within(groupRow).getByRole('combobox', { name: `${t.modelVisibility}: fast` });
    expect(combo).toHaveTextContent(t.modelVisibilityHidden);
    // Revert the group back to Shown → setModelVisibility called with the group name.
    fireEvent.mouseDown(combo);
    fireEvent.click(await screen.findByRole('option', { name: t.modelVisibilityShown }));
    await waitFor(() => expect(setModelVisibility).toHaveBeenCalledWith('fast', 'shown'));
    expect(onModelsChanged).toHaveBeenCalled();
    // A real model row still has its own visibility select.
    expect(
      screen.getByRole('combobox', { name: `${t.modelVisibility}: llama3` }),
    ).toBeInTheDocument();
  });

  it('non-admins see a read-only group chip + visibility chip (no select)', () => {
    const withGroup: ModelOption[] = [
      {
        id: 'fast',
        display_name: 'fast',
        flavors: ['openai'],
        is_group: true,
        visibility: 'locked',
      },
    ];
    renderList({ t, models: withGroup });
    const groupRow = screen.getAllByText('fast')[0].closest('tr')!;
    expect(within(groupRow).getByText(t.modelGroupChip)).toBeInTheDocument();
    expect(within(groupRow).getByText(t.modelVisibilityLocked)).toBeInTheDocument();
    expect(within(groupRow).queryByRole('combobox')).toBeNull();
  });

  it('shows a separate Typ column: group chip for a group, model label for a model', () => {
    const models: ModelOption[] = [
      {
        id: 'fast-group',
        display_name: 'fast-group',
        flavors: ['openai'],
        is_group: true,
        visibility: 'shown',
      },
      { id: 'llama3', display_name: 'llama3', flavors: ['openai'] },
    ];
    renderList({ t, models });
    // The dedicated Typ column header is present.
    expect(screen.getByText(t.tableModelType)).toBeInTheDocument();
    // A group row shows the group chip in its Typ cell.
    const groupRow = screen.getAllByText('fast-group')[0].closest('tr')!;
    expect(within(groupRow).getByText(t.modelGroupChip)).toBeInTheDocument();
    // A plain model row shows the "Modell" type label and NO group chip.
    const modelRow = screen.getAllByText('llama3')[0].closest('tr')!;
    expect(within(modelRow).getByText(t.tableModel)).toBeInTheDocument();
    expect(within(modelRow).queryByText(t.modelGroupChip)).toBeNull();
  });
});
