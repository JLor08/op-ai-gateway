// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor, within } from '@testing-library/react';
import { ModelGroupSection } from './ModelGroupSection';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { CreateModelGroupRequest, ModelOption, PortalModelGroup } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(() => cleanup());

const models: ModelOption[] = [
  { id: 'm1', display_name: 'm1', flavors: ['openai'] },
  { id: 'm2', display_name: 'm2', flavors: ['openai'] },
  { id: 'fast', display_name: 'Fast', flavors: [], is_group: true },
];

function makeGroup(overrides: Partial<PortalModelGroup> = {}): PortalModelGroup {
  return {
    id: 'grp_1',
    gateway_model_name: 'fast',
    display_name: 'Fast',
    status: 'active',
    failover_mode: 'sticky',
    visibility: 'shown',
    members: [{ member_gateway_name: 'm1' }, { member_gateway_name: 'm2' }],
    traversal: 'round_robin',
    loaded_only: false,
    member_order: 'priority',
    climb_speed_margin_percent: 20,
    min_tokens_per_second: 0,
    min_speed_fallback: 'error',
    ...overrides,
  };
}

function renderSection(
  opts: {
    groups?: PortalModelGroup[];
    createModelGroup?: PortalApi['createModelGroup'];
    deleteModelGroup?: PortalApi['deleteModelGroup'];
  } = {},
) {
  const groups = opts.groups ?? [];
  const created: CreateModelGroupRequest[] = [];
  const onModelsChanged = vi.fn();
  const fakeApi = {
    modelGroups: vi.fn(async () => ({ data: groups })),
    createModelGroup:
      opts.createModelGroup ??
      vi.fn(async (body: CreateModelGroupRequest) => {
        created.push(body);
        return makeGroup({ id: 'grp_new', ...(body as Partial<PortalModelGroup>) });
      }),
    updateModelGroup: vi.fn(async (id: string, body: Record<string, unknown>) =>
      makeGroup({ id, ...(body as Partial<PortalModelGroup>) }),
    ),
    deleteModelGroup: opts.deleteModelGroup ?? vi.fn(async () => ({ ok: true })),
  };

  render(
    <ToastProvider>
      <ModelGroupSection t={t} api={fakeApi} models={models} onModelsChanged={onModelsChanged} />
    </ToastProvider>,
  );
  return { fakeApi, created, onModelsChanged };
}

describe('ModelGroupSection', () => {
  it('lists existing groups', async () => {
    renderSection({ groups: [makeGroup({ gateway_model_name: 'fast' })] });
    expect(await screen.findByText('fast')).toBeInTheDocument();
  });

  it('creates a group with an ordered member', async () => {
    const { created, onModelsChanged } = renderSection();

    // Open the create form.
    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));
    fireEvent.change(screen.getByLabelText(t.modelGroupGatewayName), {
      target: { value: 'grp-1' },
    });

    // Add m1 as a member via the ordered-member picker.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupAddMember }));
    fireEvent.click(await screen.findByRole('option', { name: 'm1' }));

    // Submit (the create button in the form carries the same label).
    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0]).toMatchObject({
      gateway_model_name: 'grp-1',
      failover_mode: 'sticky',
      visibility: 'shown',
      members: [{ member_gateway_name: 'm1' }],
    });
    expect(onModelsChanged).toHaveBeenCalled();
  });

  it('creates a group with a chosen visibility (hidden)', async () => {
    const { created } = renderSection();

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));
    fireEvent.change(screen.getByLabelText(t.modelGroupGatewayName), {
      target: { value: 'grp-1' },
    });

    // Pick "hidden" in the visibility select.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelVisibility }));
    fireEvent.click(await screen.findByRole('option', { name: t.modelVisibilityHidden }));

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].visibility).toBe('hidden');
  });

  it('offers another group as a member-picker option', async () => {
    renderSection();

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupAddMember }));
    expect(
      await screen.findByRole('option', { name: `Fast (${t.modelGroupChip})` }),
    ).toBeInTheDocument();
  });

  it('defaults a fresh create to round_robin traversal', async () => {
    const { created } = renderSection();

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));
    fireEvent.change(screen.getByLabelText(t.modelGroupGatewayName), {
      target: { value: 'grp-1' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].traversal).toBe('round_robin');
  });

  it('round-trips a chosen traversal strategy into the create body', async () => {
    const { created } = renderSection();

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));
    fireEvent.change(screen.getByLabelText(t.modelGroupGatewayName), {
      target: { value: 'grp-1' },
    });

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupTraversal }));
    fireEvent.click(await screen.findByRole('option', { name: t.modelGroupTraversalBreadth }));

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].traversal).toBe('breadth');
  });

  it('pre-fills the visibility select from the group being edited', async () => {
    renderSection({
      groups: [makeGroup({ id: 'grp_1', gateway_model_name: 'fast', visibility: 'locked' })],
    });
    await screen.findByText('fast');

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupEdit }));
    const combo = await screen.findByRole('combobox', { name: t.modelVisibility });
    expect(combo).toHaveTextContent(t.modelVisibilityLocked);
  });

  it('renders the selection controls in the editor', async () => {
    renderSection();
    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));

    expect(screen.getByRole('checkbox', { name: t.modelGroupLoadedOnly })).toBeInTheDocument();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupMemberOrder }));
    expect(
      await screen.findByRole('option', { name: t.modelGroupMemberOrderPriority }),
    ).toBeInTheDocument();
    expect(screen.getByRole('option', { name: t.modelGroupMemberOrderSpeed })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('option', { name: t.modelGroupMemberOrderPriority }));

    expect(screen.getByLabelText(t.modelGroupClimbSpeedMargin)).toBeInTheDocument();
    expect(screen.getByLabelText(t.modelGroupMinTokensPerSecond)).toBeInTheDocument();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupMinSpeedFallback }));
    expect(
      await screen.findByRole('option', { name: t.modelGroupMinSpeedFallbackError }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('option', { name: t.modelGroupMinSpeedFallbackIgnore }),
    ).toBeInTheDocument();
  });

  it('sends the selection fields in the update payload, including an explicit zero margin', async () => {
    const { fakeApi } = renderSection({
      groups: [makeGroup({ id: 'grp_1', gateway_model_name: 'fast' })],
    });
    await screen.findByText('fast');

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupEdit }));

    fireEvent.click(await screen.findByRole('checkbox', { name: t.modelGroupLoadedOnly }));

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupMemberOrder }));
    fireEvent.click(await screen.findByRole('option', { name: t.modelGroupMemberOrderSpeed }));

    fireEvent.change(screen.getByLabelText(t.modelGroupClimbSpeedMargin), {
      target: { value: '0' },
    });
    fireEvent.change(screen.getByLabelText(t.modelGroupMinTokensPerSecond), {
      target: { value: '5.5' },
    });

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupMinSpeedFallback }));
    fireEvent.click(
      await screen.findByRole('option', { name: t.modelGroupMinSpeedFallbackIgnore }),
    );

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupSave }));

    await waitFor(() => expect(fakeApi.updateModelGroup).toHaveBeenCalled());
    const [, body] = fakeApi.updateModelGroup.mock.calls[0];
    expect(body).toMatchObject({
      loaded_only: true,
      member_order: 'speed',
      climb_speed_margin_percent: 0,
      min_tokens_per_second: 5.5,
      min_speed_fallback: 'ignore',
    });
  });

  it('omits the margin from the payload when the field is left blank', async () => {
    const { created } = renderSection();

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));
    fireEvent.change(screen.getByLabelText(t.modelGroupGatewayName), {
      target: { value: 'grp-1' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0]).not.toHaveProperty('climb_speed_margin_percent');
  });

  it('shows the stored selection values for a group loaded from the API', async () => {
    renderSection({
      groups: [
        makeGroup({
          id: 'grp_1',
          gateway_model_name: 'fast',
          loaded_only: true,
          member_order: 'speed',
          climb_speed_margin_percent: 0,
          min_tokens_per_second: 12.5,
          min_speed_fallback: 'ignore',
        }),
      ],
    });
    await screen.findByText('fast');

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupEdit }));

    expect(
      (await screen.findByRole('checkbox', { name: t.modelGroupLoadedOnly })) as HTMLInputElement,
    ).toBeChecked();
    expect(screen.getByRole('combobox', { name: t.modelGroupMemberOrder })).toHaveTextContent(
      t.modelGroupMemberOrderSpeed,
    );
    expect((screen.getByLabelText(t.modelGroupClimbSpeedMargin) as HTMLInputElement).value).toBe(
      '0',
    );
    expect((screen.getByLabelText(t.modelGroupMinTokensPerSecond) as HTMLInputElement).value).toBe(
      '12.5',
    );
    expect(screen.getByRole('combobox', { name: t.modelGroupMinSpeedFallback })).toHaveTextContent(
      t.modelGroupMinSpeedFallbackIgnore,
    );
  });

  it('deletes a group after confirmation', async () => {
    const deleteModelGroup = vi.fn(async () => ({ ok: true }));
    renderSection({
      groups: [makeGroup({ id: 'grp_1', gateway_model_name: 'fast' })],
      deleteModelGroup,
    });
    await screen.findByText('fast');

    fireEvent.click(screen.getByRole('button', { name: t.modelGroupDelete }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: t.modelGroupDelete }));

    await waitFor(() => expect(deleteModelGroup).toHaveBeenCalledWith('grp_1'));
  });
});
