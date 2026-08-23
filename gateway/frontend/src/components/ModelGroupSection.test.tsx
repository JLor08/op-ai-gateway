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
