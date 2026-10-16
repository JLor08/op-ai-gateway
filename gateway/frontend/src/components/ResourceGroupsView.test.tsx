// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ResourceGroupsView } from './ResourceGroupsView';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import { PortalApiError } from '../api';
import type {
  AdminGroupCandidate,
  CreateResourceGroupRequest,
  ResourceGroup,
  ResourceGroupProvision,
  ResourceGroupProvisionCandidates,
  ResourceGroupServerRef,
  ResourceGroupVendorAccountRef,
  UpdateResourceGroupRequest,
} from '../api';
import type { PortalApi } from './shared/types';

function makeResourceGroup(overrides: Partial<ResourceGroup> = {}): ResourceGroup {
  return {
    id: 'rgrp_1',
    name: 'Test Resource Group',
    status: 'active',
    system_group: { id: 'sg_default', name: 'Default System Group' },
    admin_groups: [{ id: 'ag_default', name: 'Default Admin Group' }],
    servers: [],
    vendor_accounts: [],
    ...overrides,
  };
}

// A single admin-group candidate under a single system group (mirrors
// ServicesView.test.tsx / ServerList.test.tsx's defaultAdminGroupCandidates)
// -- the common case where the create form's picker auto-selects with no
// extra step, so tests that don't care about admin-group linkage keep the
// "Ressourcengruppe anlegen" action reachable.
const defaultAdminGroupCandidates: AdminGroupCandidate[] = [
  {
    id: 'ag_default',
    name: 'Default Admin Group',
    parent_group_id: 'sg_default',
    parent_group_name: 'Default System Group',
  },
];

afterEach(cleanup);

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderResourceGroupsView(
    opts: {
      groups?: ResourceGroup[];
      role?: string;
      // The vendor_account_sharing_enabled master flag as the shell reads it; on
      // unless a test turns it off, so the provider-account panel is there to test.
      sharingEnabled?: boolean;
      overrides?: Partial<PortalApi>;
    } = {},
  ) {
    const groups = opts.groups ?? [makeResourceGroup()];
    const fakeApi = {
      resourceGroups: vi.fn(async () => ({ data: groups })),
      createResourceGroup: vi.fn(async (body: CreateResourceGroupRequest) =>
        makeResourceGroup({
          id: 'rgrp_created',
          name: body.name,
          status: (body.status as ResourceGroup['status']) ?? 'active',
        }),
      ),
      updateResourceGroup: vi.fn(async (id: string, body: UpdateResourceGroupRequest) =>
        makeResourceGroup({
          id,
          name: body.name ?? groups[0].name,
          status: (body.status as ResourceGroup['status']) ?? groups[0].status,
        }),
      ),
      deleteResourceGroup: vi.fn(async () => ({ ok: true })),
      resourceGroupAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      setResourceGroupAdminGroups: vi.fn(async () => makeResourceGroup()),
      resourceGroupServerCandidates: vi.fn(async () => [] as ResourceGroupServerRef[]),
      setResourceGroupServers: vi.fn(async () => makeResourceGroup()),
      resourceGroupVendorAccountCandidates: vi.fn(
        async () => [] as ResourceGroupVendorAccountRef[],
      ),
      setResourceGroupVendorAccounts: vi.fn(async () => makeResourceGroup()),
      resourceGroupProvisions: vi.fn(async () => [] as ResourceGroupProvision[]),
      resourceGroupProvisionCandidates: vi.fn(
        async () =>
          ({
            users: [],
            user_groups: [],
            admin_groups: [],
            services: [],
          }) as ResourceGroupProvisionCandidates,
      ),
      setResourceGroupProvisions: vi.fn(async () => ({ ok: true })),
    };
    Object.assign(fakeApi, opts.overrides ?? {});

    render(
      <ToastProvider>
        <ResourceGroupsView
          t={t}
          api={fakeApi}
          role={opts.role ?? 'admin'}
          vendorAccountSharingEnabled={opts.sharingEnabled ?? true}
        />
      </ToastProvider>,
    );
    return { fakeApi };
  }

  describe(`ResourceGroupsView list [${locale}]`, () => {
    it('renders name/status/system-group/admin-groups/server-count columns', async () => {
      const group = makeResourceGroup({
        name: 'GPU Pool',
        status: 'disabled',
        system_group: { id: 'sg_1', name: 'Ops System' },
        admin_groups: [{ id: 'ag_1', name: 'Ops Admins' }],
        servers: [
          { id: 'srv_1', name: 'GPU 1' },
          { id: 'srv_2', name: 'GPU 2' },
        ],
      });
      renderResourceGroupsView({ groups: [group] });

      await screen.findByText('GPU Pool');
      const row = screen.getByText('GPU Pool').closest('tr')!;
      expect(within(row).getByText(t.statusDisabled)).toBeInTheDocument();
      expect(within(row).getByText('Ops System')).toBeInTheDocument();
      expect(within(row).getByText('Ops Admins')).toBeInTheDocument();
      expect(within(row).getByText('2')).toBeInTheDocument();
    });

    it('shows the create action for an admin', async () => {
      renderResourceGroupsView({ role: 'admin' });
      expect(
        await screen.findByRole('button', { name: t.resourceGroupCreate }),
      ).toBeInTheDocument();
    });

    it('hides the create action for a non-admin', async () => {
      renderResourceGroupsView({ role: 'user' });
      await screen.findByText('Test Resource Group');
      expect(screen.queryByRole('button', { name: t.resourceGroupCreate })).not.toBeInTheDocument();
    });
  });

  describe(`ResourceGroupsView create form (Resource Groups Phase 1, spec 2026-08-11) [${locale}]`, () => {
    function renderCreate(
      candidates: AdminGroupCandidate[],
      createResourceGroup?: PortalApi['createResourceGroup'],
    ) {
      const create =
        createResourceGroup ??
        vi.fn(async (body: CreateResourceGroupRequest) =>
          makeResourceGroup({ id: 'rgrp_new', name: body.name }),
        );
      const fakeApi = {
        resourceGroups: vi.fn(async () => ({ data: [] })),
        resourceGroupAdminGroupCandidates: vi.fn(async () => candidates),
        createResourceGroup: create,
        updateResourceGroup: vi.fn(async () => makeResourceGroup()),
        deleteResourceGroup: vi.fn(async () => ({ ok: true })),
        setResourceGroupAdminGroups: vi.fn(async () => makeResourceGroup()),
        resourceGroupServerCandidates: vi.fn(async () => [] as ResourceGroupServerRef[]),
        setResourceGroupServers: vi.fn(async () => makeResourceGroup()),
        resourceGroupVendorAccountCandidates: vi.fn(
          async () => [] as ResourceGroupVendorAccountRef[],
        ),
        setResourceGroupVendorAccounts: vi.fn(async () => makeResourceGroup()),
        resourceGroupProvisions: vi.fn(async () => [] as ResourceGroupProvision[]),
        resourceGroupProvisionCandidates: vi.fn(
          async () =>
            ({
              users: [],
              user_groups: [],
              admin_groups: [],
              services: [],
            }) as ResourceGroupProvisionCandidates,
        ),
        setResourceGroupProvisions: vi.fn(async () => ({ ok: true })),
      };
      render(
        <ToastProvider>
          <ResourceGroupsView t={t} api={fakeApi} role="admin" vendorAccountSharingEnabled />
        </ToastProvider>,
      );
      return { createResourceGroup: create as ReturnType<typeof vi.fn> };
    }

    it('auto-selects the single admin-group candidate (no field) and submits its id', async () => {
      const candidates: AdminGroupCandidate[] = [
        {
          id: 'ag_1',
          name: 'Ops Admins',
          parent_group_id: 'sg_1',
          parent_group_name: 'Ops System',
        },
      ];
      const { createResourceGroup } = renderCreate(candidates);

      fireEvent.click(await screen.findByRole('button', { name: t.resourceGroupCreate }));
      // The auto note names the single candidate + derives the system group;
      // no picker of any kind renders.
      await screen.findByText(t.resourceGroupAdminGroupAuto('Ops Admins'));
      expect(
        screen.getByText(t.resourceGroupAdminGroupSystemGroupAuto('Ops System')),
      ).toBeInTheDocument();
      expect(screen.queryByLabelText(t.resourceGroupAdminGroupLabel)).not.toBeInTheDocument();
      expect(
        screen.queryByLabelText(t.resourceGroupAdminGroupSystemGroupLabel),
      ).not.toBeInTheDocument();

      fireEvent.change(screen.getByLabelText(t.resourceGroupNameLabel), {
        target: { value: 'RG' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupCreate }));
      await waitFor(() => expect(createResourceGroup).toHaveBeenCalledTimes(1));
      expect(createResourceGroup.mock.calls[0][0].admin_group_ids).toEqual(['ag_1']);
    });

    it('shows a required multi-select when there are several candidates under ONE system group, and submits the chosen ids', async () => {
      const candidates: AdminGroupCandidate[] = [
        { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
        { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      ];
      const { createResourceGroup } = renderCreate(candidates);

      fireEvent.click(await screen.findByRole('button', { name: t.resourceGroupCreate }));
      // A single shared parent -> no system-group step, just the derived note.
      await screen.findByText(t.resourceGroupAdminGroupSystemGroupAuto('Ops System'));
      expect(
        screen.queryByLabelText(t.resourceGroupAdminGroupSystemGroupLabel),
      ).not.toBeInTheDocument();
      fireEvent.change(screen.getByLabelText(t.resourceGroupNameLabel), {
        target: { value: 'RG' },
      });

      // Nothing picked yet -> submit stays disabled.
      expect(screen.getByRole('button', { name: t.resourceGroupCreate })).toBeDisabled();

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupAdminGroupLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));

      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupCreate }));
      await waitFor(() => expect(createResourceGroup).toHaveBeenCalledTimes(1));
      expect(createResourceGroup.mock.calls[0][0].admin_group_ids).toEqual(['ag_b']);
    });

    it('requires a system-group choice first when candidates span MORE THAN ONE parent, then narrows the admin-group picker to its children', async () => {
      const candidates: AdminGroupCandidate[] = [
        { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'System One' },
        { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'System One' },
        { id: 'ag_c', name: 'Group C', parent_group_id: 'sg_2', parent_group_name: 'System Two' },
      ];
      const { createResourceGroup } = renderCreate(candidates);

      fireEvent.click(await screen.findByRole('button', { name: t.resourceGroupCreate }));
      await screen.findByLabelText(t.resourceGroupAdminGroupSystemGroupLabel);
      // No admin-group picker of any kind before a system group is chosen.
      expect(screen.queryByLabelText(t.resourceGroupAdminGroupLabel)).not.toBeInTheDocument();
      expect(screen.queryByText(t.resourceGroupAdminGroupAuto('Group C'))).not.toBeInTheDocument();

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupAdminGroupSystemGroupLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'System Two' }));

      // Narrowed to System Two's single child -> auto-selected.
      await screen.findByText(t.resourceGroupAdminGroupAuto('Group C'));
      expect(screen.queryByLabelText(t.resourceGroupAdminGroupLabel)).not.toBeInTheDocument();
      expect(screen.queryByText('Group A')).not.toBeInTheDocument();

      fireEvent.change(screen.getByLabelText(t.resourceGroupNameLabel), {
        target: { value: 'RG' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupCreate }));
      await waitFor(() => expect(createResourceGroup).toHaveBeenCalledTimes(1));
      expect(createResourceGroup.mock.calls[0][0].admin_group_ids).toEqual(['ag_c']);
    });

    it('shows a hint and keeps the submit action disabled when the caller has no admin-group candidate', async () => {
      const createResourceGroup = vi.fn(async (body: CreateResourceGroupRequest) =>
        makeResourceGroup({ id: 'rgrp_new', name: body.name }),
      );
      renderCreate([], createResourceGroup as unknown as PortalApi['createResourceGroup']);

      fireEvent.click(await screen.findByRole('button', { name: t.resourceGroupCreate }));
      await screen.findByText(t.resourceGroupNoAdminGroupHint);
      fireEvent.change(screen.getByLabelText(t.resourceGroupNameLabel), {
        target: { value: 'RG' },
      });

      expect(screen.getByRole('button', { name: t.resourceGroupCreate })).toBeDisabled();
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupCreate }));
      expect(createResourceGroup).not.toHaveBeenCalled();
    });
  });

  describe(`ResourceGroupsView edit-form admin-groups editor (fixed root -- a resource group always has one after create) [${locale}]`, () => {
    it("pre-fills the group's linked admin groups and saves the edited set via its own button", async () => {
      const group = makeResourceGroup({
        id: 'rgrp_ag',
        admin_groups: [{ id: 'ag_a', name: 'Group A' }],
        system_group: { id: 'sg_1', name: 'Ops System' },
      });
      const candidates: AdminGroupCandidate[] = [
        { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
        { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
        // A candidate under a DIFFERENT system group must NOT be offered here.
        {
          id: 'ag_other',
          name: "Other System's Group",
          parent_group_id: 'sg_2',
          parent_group_name: 'Other System',
        },
      ];
      const setResourceGroupAdminGroups = vi.fn(async () => ({
        ...group,
        admin_groups: [{ id: 'ag_b', name: 'Group B' }],
      }));
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupAdminGroupCandidates: vi.fn(async () => candidates),
          setResourceGroupAdminGroups,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.resourceGroupAdminGroupsSectionTitle);
      // Pre-filled with the group's own linked group.
      expect(screen.getByText('Group A')).toBeInTheDocument();
      expect(screen.queryByText("Other System's Group")).not.toBeInTheDocument();

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupAdminGroupLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupAdminGroupsSave }));

      await waitFor(() =>
        expect(setResourceGroupAdminGroups).toHaveBeenCalledWith('rgrp_ag', ['ag_a', 'ag_b']),
      );
      expect(await screen.findByText(t.resourceGroupAdminGroupsSaved)).toBeInTheDocument();
    });
  });

  describe(`ResourceGroupsView server editor (Resource Groups Phase 1, spec 2026-08-11) [${locale}]`, () => {
    it("pre-fills the group's current servers, adds a candidate, and saves via setResourceGroupServers", async () => {
      const group = makeResourceGroup({
        id: 'rgrp_srv',
        servers: [{ id: 'srv_1', name: 'GPU 1' }],
      });
      const serverCandidates: ResourceGroupServerRef[] = [
        { id: 'srv_1', name: 'GPU 1' },
        { id: 'srv_2', name: 'GPU 2' },
      ];
      const setResourceGroupServers = vi.fn(async () => ({
        ...group,
        servers: [
          { id: 'srv_1', name: 'GPU 1' },
          { id: 'srv_2', name: 'GPU 2' },
        ],
      }));
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupServerCandidates: vi.fn(async () => serverCandidates),
          setResourceGroupServers,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.resourceGroupServersSectionTitle);
      expect(screen.getByText('GPU 1')).toBeInTheDocument();

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupServersLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'GPU 2' }));
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupServersSave }));

      await waitFor(() =>
        expect(setResourceGroupServers).toHaveBeenCalledWith('rgrp_srv', ['srv_1', 'srv_2']),
      );
      expect(await screen.findByText(t.resourceGroupServersSaved)).toBeInTheDocument();
    });

    it('surfaces a toast when adding a server fails with a system-group-mismatch error', async () => {
      const group = makeResourceGroup({ id: 'rgrp_mismatch', servers: [] });
      const serverCandidates: ResourceGroupServerRef[] = [{ id: 'srv_x', name: 'GPU X' }];
      const setResourceGroupServers = vi.fn(async () => {
        throw new PortalApiError(
          400,
          'resource_group.server_system_group_mismatch',
          "server must belong to the resource group's system group",
        );
      });
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupServerCandidates: vi.fn(async () => serverCandidates),
          setResourceGroupServers,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.resourceGroupServersSectionTitle);

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupServersLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'GPU X' }));
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupServersSave }));

      const alert = await screen.findByRole('alert');
      expect(alert.textContent ?? '').toContain('resource_group.server_system_group_mismatch');
    });
  });

  // The provider-account ("Anbieter") panel (#183, spec 2026-10-15 sections 9 and 14):
  // the resource-group MANAGER side of a shared vendor account, mirroring the server
  // editor above. The group's current links come from its own DTO; the candidates are
  // what the manager may ADD (full accounts under the group's root, which the manager
  // operates) -- not the whole selection, since the group may also hold links the
  // manager could not add themselves (an owner's `shared` link, an account they no
  // longer operate) that they may still drop.
  describe(`ResourceGroupsView provider-account panel (#183, spec 2026-10-15) [${locale}]`, () => {
    const vendorAccountsRegion = () =>
      screen.getByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle });

    it("pre-fills the group's current accounts, adds a candidate, and saves via setResourceGroupVendorAccounts", async () => {
      // va_1 is linked but is NOT among the candidates (e.g. the owner linked it at
      // Shared): it must still be shown AND kept when the manager adds another one.
      const group = makeResourceGroup({
        id: 'rgrp_va',
        vendor_accounts: [{ id: 'va_1', name: 'Team OpenAI' }],
      });
      const candidates: ResourceGroupVendorAccountRef[] = [
        { id: 'va_2', name: 'Team Anthropic' },
        { id: 'va_3', name: 'Team Gemini' },
      ];
      const resourceGroupVendorAccountCandidates = vi.fn(async () => candidates);
      const setResourceGroupVendorAccounts = vi.fn(async () => ({
        ...group,
        vendor_accounts: [
          { id: 'va_1', name: 'Team OpenAI' },
          { id: 'va_2', name: 'Team Anthropic' },
        ],
      }));
      renderResourceGroupsView({
        groups: [group],
        overrides: { resourceGroupVendorAccountCandidates, setResourceGroupVendorAccounts },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle });
      await waitFor(() =>
        expect(resourceGroupVendorAccountCandidates).toHaveBeenCalledWith('rgrp_va'),
      );
      expect(within(vendorAccountsRegion()).getByText('Team OpenAI')).toBeInTheDocument();

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupVendorAccountsLabel));
      // The listbox offers the candidates and the accounts already linked are not
      // offered a second time (they are the selection).
      expect(await screen.findByRole('option', { name: 'Team Gemini' })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: 'Team OpenAI' })).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('option', { name: 'Team Anthropic' }));
      fireEvent.click(
        within(vendorAccountsRegion()).getByRole('button', {
          name: t.resourceGroupVendorAccountsSave,
        }),
      );

      await waitFor(() =>
        expect(setResourceGroupVendorAccounts).toHaveBeenCalledWith('rgrp_va', ['va_1', 'va_2']),
      );
      expect(await screen.findByText(t.resourceGroupVendorAccountsSaved)).toBeInTheDocument();
    });

    it('drops a linked account the manager removes, sending the remaining set', async () => {
      const group = makeResourceGroup({
        id: 'rgrp_drop',
        vendor_accounts: [
          { id: 'va_1', name: 'Team OpenAI' },
          { id: 'va_2', name: 'Team Anthropic' },
        ],
      });
      const setResourceGroupVendorAccounts = vi.fn(async () => ({
        ...group,
        vendor_accounts: [{ id: 'va_2', name: 'Team Anthropic' }],
      }));
      renderResourceGroupsView({ groups: [group], overrides: { setResourceGroupVendorAccounts } });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle });
      const chip = within(vendorAccountsRegion()).getByText('Team OpenAI').closest('.MuiChip-root');
      fireEvent.click(within(chip as HTMLElement).getByTestId('CancelIcon'));
      fireEvent.click(
        within(vendorAccountsRegion()).getByRole('button', {
          name: t.resourceGroupVendorAccountsSave,
        }),
      );

      await waitFor(() =>
        expect(setResourceGroupVendorAccounts).toHaveBeenCalledWith('rgrp_drop', ['va_2']),
      );
    });

    it('still lets the manager see and drop the current links when the candidates cannot be loaded', async () => {
      // The candidate GET is a 409 while the module or the sharing switch is off.
      const group = makeResourceGroup({
        id: 'rgrp_off',
        vendor_accounts: [{ id: 'va_1', name: 'Team OpenAI' }],
      });
      const setResourceGroupVendorAccounts = vi.fn(async () => ({ ...group, vendor_accounts: [] }));
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupVendorAccountCandidates: vi.fn(async () => {
            throw new PortalApiError(409, 'vendor_accounts.sharing_disabled', 'sharing is off');
          }),
          setResourceGroupVendorAccounts,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle });
      expect(within(vendorAccountsRegion()).getByText('Team OpenAI')).toBeInTheDocument();

      fireEvent.click(within(vendorAccountsRegion()).getByTestId('CancelIcon'));
      fireEvent.click(
        within(vendorAccountsRegion()).getByRole('button', {
          name: t.resourceGroupVendorAccountsSave,
        }),
      );
      await waitFor(() =>
        expect(setResourceGroupVendorAccounts).toHaveBeenCalledWith('rgrp_off', []),
      );
    });

    it('surfaces a refused add as a toast and keeps the selection so the manager can adjust', async () => {
      const group = makeResourceGroup({ id: 'rgrp_refused', vendor_accounts: [] });
      const setResourceGroupVendorAccounts = vi.fn(async () => {
        throw new PortalApiError(403, 'vendor_account.forbidden', 'not allowed');
      });
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupVendorAccountCandidates: vi.fn(async () => [{ id: 'va_x', name: 'Team X' }]),
          setResourceGroupVendorAccounts,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle });
      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupVendorAccountsLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Team X' }));
      fireEvent.click(
        within(vendorAccountsRegion()).getByRole('button', {
          name: t.resourceGroupVendorAccountsSave,
        }),
      );

      const alert = await screen.findByRole('alert');
      expect(alert.textContent ?? '').toContain('vendor_account.forbidden');
      expect(alert.textContent ?? '').toContain(t.errorVendorAccountForbidden);
      expect(within(vendorAccountsRegion()).getByText('Team X')).toBeInTheDocument();
    });

    it('labels a root-mismatch refusal in the active locale, not the raw server text', async () => {
      const group = makeResourceGroup({ id: 'rgrp_mismatch_va', vendor_accounts: [] });
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupVendorAccountCandidates: vi.fn(async () => [{ id: 'va_x', name: 'Team X' }]),
          setResourceGroupVendorAccounts: vi.fn(async () => {
            throw new PortalApiError(
              400,
              'resource_group.vendor_account_system_group_mismatch',
              "vendor account must belong to the resource group's system group",
            );
          }),
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle });
      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupVendorAccountsLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Team X' }));
      fireEvent.click(
        within(vendorAccountsRegion()).getByRole('button', {
          name: t.resourceGroupVendorAccountsSave,
        }),
      );

      const alert = await screen.findByRole('alert');
      expect(alert.textContent ?? '').toContain(
        t.errorResourceGroupVendorAccountSystemGroupMismatch,
      );
    });
  });

  // While vendor_account_sharing_enabled is off the backend answers every
  // resource-group vendor-account endpoint 409 (the candidates and the full-replace
  // PUT alike), so the manager is not offered the panel at all and nothing is
  // fetched for it. The group's stored links are paused, not deleted. The rest of the
  // detail view (servers, provisions) is unaffected.
  describe(`ResourceGroupsView provider-account panel with sharing switched off [${locale}]`, () => {
    it('hides the panel and requests no candidates, while the other sections stay', async () => {
      const group = makeResourceGroup({
        id: 'rgrp_off',
        vendor_accounts: [{ id: 'va_1', name: 'Team OpenAI' }],
      });
      const { fakeApi } = renderResourceGroupsView({ groups: [group], sharingEnabled: false });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.resourceGroupServersSectionTitle);
      await screen.findByText(t.resourceGroupProvisionsSectionTitle);

      expect(
        screen.queryByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle }),
      ).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.resourceGroupVendorAccountsLabel)).not.toBeInTheDocument();
      expect(screen.queryByText('Team OpenAI')).not.toBeInTheDocument();
      expect(fakeApi.resourceGroupVendorAccountCandidates).not.toHaveBeenCalled();
      // The servers panel still loads its own candidates.
      await waitFor(() =>
        expect(fakeApi.resourceGroupServerCandidates).toHaveBeenCalledWith('rgrp_off'),
      );
    });

    it('shows the panel and loads its candidates with the flag on', async () => {
      const group = makeResourceGroup({ id: 'rgrp_on', vendor_accounts: [] });
      const { fakeApi } = renderResourceGroupsView({ groups: [group], sharingEnabled: true });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      expect(
        await screen.findByRole('region', { name: t.resourceGroupVendorAccountsSectionTitle }),
      ).toBeInTheDocument();
      await waitFor(() =>
        expect(fakeApi.resourceGroupVendorAccountCandidates).toHaveBeenCalledWith('rgrp_on'),
      );
      expect(fakeApi.setResourceGroupVendorAccounts).not.toHaveBeenCalled();
    });
  });

  describe(`ResourceGroupsView provisioning editor (Phase 2, spec 2026-08-12-resource-groups-phase-2-provisioning) [${locale}]`, () => {
    it('lists current provisions, adds a target of each kind, removes one, and saves the updated set', async () => {
      const group = makeResourceGroup({ id: 'rgrp_prov' });
      const candidates: ResourceGroupProvisionCandidates = {
        users: [{ id: 'usr_2', email: 'bob@example.com', display_name: 'Bob' }],
        user_groups: [{ id: 'ug_1', name: 'UG One' }],
        admin_groups: [{ id: 'ag_1', name: 'AG One' }],
        services: [{ id: 'svc_1', name: 'Service One' }],
      };
      const setResourceGroupProvisions = vi.fn(async () => ({ ok: true }));
      let provisionsCall = 0;
      const resourceGroupProvisions = vi.fn(async () => {
        provisionsCall += 1;
        // The initial load returns one existing provision (a user); the
        // post-save refetch returns the final saved set (a stand-in for the
        // server's response -- the editor's own PUT body is asserted below).
        if (provisionsCall === 1)
          return [{ kind: 'user' as const, target_id: 'usr_1', target_name: 'Alice' }];
        return [
          { kind: 'user' as const, target_id: 'usr_2', target_name: 'Bob' },
          { kind: 'user_group' as const, target_id: 'ug_1', target_name: 'UG One' },
          { kind: 'admin_group' as const, target_id: 'ag_1', target_name: 'AG One' },
          { kind: 'service' as const, target_id: 'svc_1', target_name: 'Service One' },
        ];
      });
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupProvisionCandidates: vi.fn(async () => candidates),
          resourceGroupProvisions,
          setResourceGroupProvisions,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.resourceGroupProvisionsSectionTitle);
      // The existing provision is listed, labeled by kind (scoped to its row --
      // the add-kind selector's closed display also shows the same "Benutzer"
      // text as its default value).
      const aliceRow = screen.getByText('Alice').closest('tr')!;
      expect(within(aliceRow).getByText(t.resourceGroupProvisionKindUser)).toBeInTheDocument();

      // Add one target of each kind, switching the kind selector each time.
      async function addTarget(kindOption: string, targetOption: string) {
        fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupProvisionsAddKindLabel));
        fireEvent.click(await screen.findByRole('option', { name: kindOption }));
        fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupProvisionsAddTargetLabel));
        fireEvent.click(await screen.findByRole('option', { name: targetOption }));
        fireEvent.click(screen.getByRole('button', { name: t.resourceGroupProvisionsAddAction }));
      }
      // "user" is the default kind -- no need to switch for Bob.
      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupProvisionsAddTargetLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Bob' }));
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupProvisionsAddAction }));
      await screen.findByText('Bob');

      await addTarget(t.resourceGroupProvisionKindUserGroup, 'UG One');
      await screen.findByText('UG One');
      await addTarget(t.resourceGroupProvisionKindAdminGroup, 'AG One');
      await screen.findByText('AG One');
      await addTarget(t.resourceGroupProvisionKindService, 'Service One');
      await screen.findByText('Service One');

      // Remove the original provision.
      fireEvent.click(screen.getByText('Alice').closest('tr')!.querySelector('button')!);
      expect(screen.queryByText('Alice')).not.toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupProvisionsSave }));
      await waitFor(() =>
        expect(setResourceGroupProvisions).toHaveBeenCalledWith('rgrp_prov', [
          { kind: 'user', target_id: 'usr_2' },
          { kind: 'user_group', target_id: 'ug_1' },
          { kind: 'admin_group', target_id: 'ag_1' },
          { kind: 'service', target_id: 'svc_1' },
        ]),
      );
      expect(await screen.findByText(t.resourceGroupProvisionsSaved)).toBeInTheDocument();
    });

    it('surfaces a toast when saving is rejected with an invalid-target error', async () => {
      const group = makeResourceGroup({ id: 'rgrp_prov_err' });
      const candidates: ResourceGroupProvisionCandidates = {
        users: [{ id: 'usr_x', email: 'x@example.com', display_name: 'X' }],
        user_groups: [],
        admin_groups: [],
        services: [],
      };
      const setResourceGroupProvisions = vi.fn(async () => {
        throw new PortalApiError(
          400,
          'resource_group.provision_target_invalid',
          'target is not visible to the caller',
        );
      });
      renderResourceGroupsView({
        groups: [group],
        overrides: {
          resourceGroupProvisionCandidates: vi.fn(async () => candidates),
          resourceGroupProvisions: vi.fn(async () => []),
          setResourceGroupProvisions,
        },
      });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.resourceGroupProvisionsSectionTitle);

      fireEvent.mouseDown(screen.getByLabelText(t.resourceGroupProvisionsAddTargetLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'X' }));
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupProvisionsAddAction }));
      fireEvent.click(screen.getByRole('button', { name: t.resourceGroupProvisionsSave }));

      const alert = await screen.findByRole('alert');
      expect(alert.textContent ?? '').toContain('resource_group.provision_target_invalid');
    });
  });

  describe(`ResourceGroupsView settings + delete [${locale}]`, () => {
    it('saves an edited name/status', async () => {
      const group = makeResourceGroup({ id: 'rgrp_edit', name: 'Edit Me' });
      const { fakeApi } = renderResourceGroupsView({ groups: [group] });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      const nameField = await screen.findByLabelText(t.resourceGroupNameLabel);
      fireEvent.change(nameField, { target: { value: 'Renamed' } });
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() =>
        expect(fakeApi.updateResourceGroup).toHaveBeenCalledWith(
          'rgrp_edit',
          expect.objectContaining({ name: 'Renamed' }),
        ),
      );
    });

    it('deletes a resource group after confirmation', async () => {
      const group = makeResourceGroup({ id: 'rgrp_del' });
      const { fakeApi } = renderResourceGroupsView({ groups: [group] });

      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      fireEvent.click(await screen.findByRole('button', { name: t.resourceGroupActionDelete }));
      expect(fakeApi.deleteResourceGroup).not.toHaveBeenCalled();
      const dialog = within(screen.getByRole('dialog'));
      fireEvent.click(dialog.getByRole('button', { name: t.resourceGroupActionDelete }));

      await waitFor(() => expect(fakeApi.deleteResourceGroup).toHaveBeenCalledWith('rgrp_del'));
    });
  });
}
