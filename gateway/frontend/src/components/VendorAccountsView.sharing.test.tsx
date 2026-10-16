// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Wiring tests for the Anbieter "share" area of the vendor-account detail view
// (#183, spec 2026-10-15 sections 6, 10 and 14): the share-level control, the
// management-group editor, the per-tier credential panels, what a co-manager of
// a shared SUBSCRIPTION account is not offered, and the minimal read-only view
// of a member. The backend is the gate; these pin that the UI mirrors it and
// never OFFERS an action the viewer could not take.

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VendorAccountsView } from './VendorAccountsView';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import { PortalApiError } from '../api';
import type { AdminGroupCandidate, VendorAccount, VendorAccountUsage } from '../api';
import type { PortalApi } from './shared/types';

// The zero time.Time a Go DTO serializes for a timestamp it withholds from a member.
const ZERO_TIME = '0001-01-01T00:00:00Z';

function makeVendorAccount(overrides: Partial<VendorAccount> = {}): VendorAccount {
  return {
    id: 'va_1',
    vendor: 'openai',
    auth_type: 'api_key',
    name: 'Work OpenAI',
    status: 'active',
    model_prefix: '',
    base_url: '',
    api_key_set: true,
    subscription_connected: false,
    models: [],
    created_at: '2026-09-01T12:00:00Z',
    updated_at: '2026-09-01T12:00:00Z',
    share_level: 'none',
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    is_owner: true,
    shared_by: '',
    ...overrides,
  };
}

const GROUP_A: AdminGroupCandidate = {
  id: 'ag_a',
  name: 'Group A',
  parent_group_id: 'sg_1',
  parent_group_name: 'Ops System',
};
const GROUP_B: AdminGroupCandidate = {
  id: 'ag_b',
  name: 'Group B',
  parent_group_id: 'sg_1',
  parent_group_name: 'Ops System',
};
// Under ANOTHER system group: never offered for an account rooted in sg_1.
const GROUP_OTHER: AdminGroupCandidate = {
  id: 'ag_other',
  name: "Other System's Group",
  parent_group_id: 'sg_2',
  parent_group_name: 'Other System',
};

// An account the owner has shared (or fully shared) with Group A.
const shared = (overrides: Partial<VendorAccount> = {}) =>
  makeVendorAccount({
    share_level: 'shared',
    admin_groups: [{ id: 'ag_a', name: 'Group A' }],
    system_group_id: 'sg_1',
    system_group_name: 'Ops System',
    ...overrides,
  });
// The same account as a co-manager reads it: not theirs, key-free, owner named.
const managed = (overrides: Partial<VendorAccount> = {}) =>
  shared({ is_owner: false, shared_by: 'Olga Owner', ...overrides });
// The MINIMAL view a member reads: share_level, groups, root, base_url, usage, the
// upstream model id and the timestamps all come back as zero values.
const member = (overrides: Partial<VendorAccount> = {}) =>
  makeVendorAccount({
    is_owner: false,
    shared_by: 'Olga Owner',
    share_level: '',
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    base_url: '',
    created_at: ZERO_TIME,
    updated_at: ZERO_TIME,
    models: [
      {
        gateway_model: 'team/gpt-6',
        upstream_model: '',
        api_flavor: 'openai_responses',
        display_name: 'GPT-6',
      },
    ],
    ...overrides,
  });

// A connected OpenAI subscription: the account whose usage the gateway can pull.
const OPENAI_SUBSCRIPTION: Partial<VendorAccount> = {
  id: 'va_oai_sub',
  vendor: 'openai',
  auth_type: 'subscription',
  name: 'ChatGPT Plus',
  api_key_set: false,
  subscription_connected: true,
};
// An OpenRouter api key: the api-key account the gateway can pull usage for.
const OPENROUTER_KEY: Partial<VendorAccount> = {
  id: 'va_or',
  vendor: 'openrouter',
  auth_type: 'api_key',
  name: 'Router key',
  api_key_set: true,
  base_url: 'https://openrouter.ai/api',
};

const USAGE: VendorAccountUsage = {
  five_hour_pct: 42,
  five_hour_reset_at: null,
  weekly_pct: 7,
  weekly_reset_at: null,
  credit_balance: '',
  spend_unit: '',
  spend_limit: '',
  spend_used: '',
  spend_remaining: '',
  spend_used_pct: -1,
  spend_reset_at: null,
  credit_status: '',
  updated_at: '2026-10-15T10:00:00Z',
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  const raiseTo = (level: 'Shared' | 'Full') =>
    t.vendorAccountShareRaiseAction(
      level === 'Shared' ? t.vendorAccountShareLevelShared : t.vendorAccountShareLevelFull,
    );
  const lowerToShared = t.vendorAccountShareLowerAction(t.vendorAccountShareLevelShared);

  function renderView(
    opts: {
      accounts?: VendorAccount[];
      isAdmin?: boolean;
      candidates?: AdminGroupCandidate[];
      setVendorAccountShareLevel?: PortalApi['setVendorAccountShareLevel'];
      setVendorAccountAdminGroups?: PortalApi['setVendorAccountAdminGroups'];
      updateVendorAccount?: PortalApi['updateVendorAccount'];
    } = {},
  ) {
    const accounts = opts.accounts ?? [makeVendorAccount()];
    const byId = (id: string) => accounts.find((a) => a.id === id) ?? accounts[0];
    const fakeApi = {
      vendorAccounts: vi.fn(async () => ({ data: accounts })),
      createVendorAccount: vi.fn<PortalApi['createVendorAccount']>(async () => accounts[0]),
      updateVendorAccount: vi.fn<PortalApi['updateVendorAccount']>(
        opts.updateVendorAccount ??
          (async (id, body) => ({ ...byId(id), name: body.name ?? byId(id).name })),
      ),
      deleteVendorAccount: vi.fn<PortalApi['deleteVendorAccount']>(async () => ({ ok: true })),
      connectVendorAccountImport: vi.fn<PortalApi['connectVendorAccountImport']>(async (id) =>
        byId(id),
      ),
      beginVendorAccountConnect: vi.fn<PortalApi['beginVendorAccountConnect']>(async () => ({
        authorize_url: 'https://claude.ai/oauth/authorize',
      })),
      completeVendorAccountConnect: vi.fn<PortalApi['completeVendorAccountConnect']>(async (id) =>
        byId(id),
      ),
      beginVendorAccountDeviceConnect: vi.fn<PortalApi['beginVendorAccountDeviceConnect']>(
        async () => ({ user_code: 'ABCD-EFGH', verification_url: 'https://auth.example/device' }),
      ),
      pollVendorAccountDeviceConnect: vi.fn<PortalApi['pollVendorAccountDeviceConnect']>(
        async () => ({ connected: false }),
      ),
      // The single read carries the usage snapshot (the management view only).
      vendorAccount: vi.fn<PortalApi['vendorAccount']>(async (id) => ({
        ...byId(id),
        usage: USAGE,
      })),
      testConnection: vi.fn<PortalApi['testConnection']>(async () => ({
        status: 'valid',
        detail: '',
        checked_at: '2026-10-15T10:00:00Z',
      })),
      refreshModels: vi.fn<PortalApi['refreshModels']>(async (id) => ({
        account: byId(id),
        refresh: { status: 'ok', discovered: 1, detail: '' },
      })),
      // The on-view pull defaults to the server's TTL skipping the vendor.
      refreshUsage: vi.fn<PortalApi['refreshUsage']>(async () => ({
        usage: null,
        refresh: { status: 'fresh', detail: '' },
      })),
      // The server decides raise vs lower; this fake just applies the level.
      setVendorAccountShareLevel: vi.fn<PortalApi['setVendorAccountShareLevel']>(
        opts.setVendorAccountShareLevel ??
          (async (id, level, adminGroupIds) => {
            const account = byId(id);
            if (level === 'none') {
              return {
                ...account,
                share_level: 'none',
                admin_groups: [],
                system_group_id: '',
                system_group_name: '',
              };
            }
            const named = adminGroupIds ?? account.admin_groups.map((g) => g.id);
            const all = [GROUP_A, GROUP_B];
            return {
              ...account,
              share_level: level,
              admin_groups: all.filter((g) => named.includes(g.id)).map((g) => ({ ...g })),
              system_group_id: 'sg_1',
              system_group_name: 'Ops System',
            };
          }),
      ),
      setVendorAccountAdminGroups: vi.fn<PortalApi['setVendorAccountAdminGroups']>(
        opts.setVendorAccountAdminGroups ??
          (async (id, groupIds) => ({
            ...byId(id),
            admin_groups: [GROUP_A, GROUP_B]
              .filter((g) => groupIds.includes(g.id))
              .map(({ id: gid, name }) => ({ id: gid, name })),
          })),
      ),
      vendorAccountAdminGroupCandidates: vi.fn<PortalApi['vendorAccountAdminGroupCandidates']>(
        async () => opts.candidates ?? [GROUP_A],
      ),
    };
    const view = render(
      <ToastProvider>
        <VendorAccountsView t={t} api={fakeApi} isAdmin={opts.isAdmin ?? false} />
      </ToastProvider>,
    );
    return { fakeApi, ...view };
  }

  async function openDetail(name: string = t.modelDetailsAction) {
    fireEvent.click(await screen.findByRole('button', { name }));
  }

  // The panel regions of the detail view, by their (localized) headings.
  const shareRegion = () => screen.queryByRole('region', { name: t.vendorAccountShareTitle });
  const groupsRegion = () =>
    screen.queryByRole('region', { name: t.vendorAccountAdminGroupsSectionTitle });

  describe(`VendorAccountsView share-level control [${locale}]`, () => {
    it('lets an admin owner raise a personal account to Shared, naming its management group', async () => {
      const { fakeApi } = renderView({ isAdmin: true });
      await openDetail();

      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));
      expect(panel.getByLabelText(t.vendorAccountShareLevelLabel)).toHaveValue(
        t.vendorAccountShareLevelNone,
      );
      // One candidate: auto-selected, like the server create form.
      await panel.findByText(t.vendorAccountAdminGroupAuto('Group A'));

      fireEvent.click(panel.getByRole('button', { name: raiseTo('Shared') }));

      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_1', 'shared', ['ag_a']),
      );
      expect(await screen.findByText(t.vendorAccountShareSaved)).toBeInTheDocument();
      // The view now reads the shared account the server answered with.
      await waitFor(() =>
        expect(within(shareRegion()!).getByLabelText(t.vendorAccountShareLevelLabel)).toHaveValue(
          t.vendorAccountShareLevelShared,
        ),
      );
    });

    it('offers Fully shared as the other raise target and sends it', async () => {
      const { fakeApi } = renderView({ isAdmin: true });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));
      await panel.findByText(t.vendorAccountAdminGroupAuto('Group A'));

      // Each target's meaning is spelled out before the click: Fully shared hands the
      // co-managers the credential.
      expect(panel.getByText(t.vendorAccountShareLevelSharedNote)).toBeInTheDocument();
      fireEvent.mouseDown(panel.getByLabelText(t.vendorAccountShareTargetLabel));
      fireEvent.click(await screen.findByRole('option', { name: t.vendorAccountShareLevelFull }));
      expect(panel.getByText(t.vendorAccountShareLevelFullNote)).toBeInTheDocument();
      fireEvent.click(panel.getByRole('button', { name: raiseTo('Full') }));

      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_1', 'full', ['ag_a']),
      );
    });

    it('never offers Fully shared for a subscription account, and says why', async () => {
      const { fakeApi } = renderView({
        isAdmin: true,
        accounts: [makeVendorAccount(OPENAI_SUBSCRIPTION)],
      });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));
      await panel.findByText(t.vendorAccountAdminGroupAuto('Group A'));

      // No target choice at all, and no raise to Fully shared.
      expect(panel.queryByLabelText(t.vendorAccountShareTargetLabel)).not.toBeInTheDocument();
      expect(panel.queryByRole('button', { name: raiseTo('Full') })).not.toBeInTheDocument();
      expect(panel.getByText(t.vendorAccountShareSubscriptionNote)).toBeInTheDocument();

      fireEvent.click(panel.getByRole('button', { name: raiseTo('Shared') }));
      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_oai_sub', 'shared', [
          'ag_a',
        ]),
      );
    });

    it('requires at least one management group: disabled until one of several is chosen', async () => {
      const { fakeApi } = renderView({ isAdmin: true, candidates: [GROUP_A, GROUP_B] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      const raise = panel.getByRole('button', { name: raiseTo('Shared') });
      await panel.findByLabelText(t.vendorAccountAdminGroupLabel);
      expect(raise).toBeDisabled();

      fireEvent.mouseDown(panel.getByLabelText(t.vendorAccountAdminGroupLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));
      expect(raise).toBeEnabled();

      fireEvent.click(raise);
      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_1', 'shared', ['ag_b']),
      );
    });

    it('cannot raise without a candidate group: shows the hint and keeps the raise disabled', async () => {
      renderView({ isAdmin: true, candidates: [] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      expect(await panel.findByText(t.vendorAccountNoAdminGroupHint)).toBeInTheDocument();
      expect(panel.getByRole('button', { name: raiseTo('Shared') })).toBeDisabled();
    });

    it('surfaces a refused raise as a localized toast and keeps the account as it was', async () => {
      const setVendorAccountShareLevel = vi.fn(async () => {
        throw new PortalApiError(409, 'vendor_accounts.sharing_disabled', 'raw server text');
      });
      renderView({ isAdmin: true, setVendorAccountShareLevel });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));
      await panel.findByText(t.vendorAccountAdminGroupAuto('Group A'));

      fireEvent.click(panel.getByRole('button', { name: raiseTo('Shared') }));

      expect(
        await screen.findByText(
          `vendor_accounts.sharing_disabled: ${t.errorVendorAccountsSharingDisabled}`,
        ),
      ).toBeInTheDocument();
      expect(panel.getByLabelText(t.vendorAccountShareLevelLabel)).toHaveValue(
        t.vendorAccountShareLevelNone,
      );
    });

    it('shows a plain (non-admin) owner of a personal account no share area and fetches no candidates', async () => {
      const { fakeApi } = renderView({ isAdmin: false });
      await openDetail();
      await screen.findByText(t.vendorAccountSettingsTitle);

      expect(shareRegion()).not.toBeInTheDocument();
      expect(groupsRegion()).not.toBeInTheDocument();
      expect(fakeApi.vendorAccountAdminGroupCandidates).not.toHaveBeenCalled();
    });

    it('offers the owner of a Shared account the raise to Fully shared, carrying its current groups', async () => {
      const { fakeApi } = renderView({ isAdmin: true, accounts: [shared()] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      expect(panel.getByLabelText(t.vendorAccountShareLevelLabel)).toHaveValue(
        t.vendorAccountShareLevelShared,
      );
      // Raising only ever goes up: no raise to the level it already has.
      expect(panel.queryByRole('button', { name: raiseTo('Shared') })).not.toBeInTheDocument();
      fireEvent.click(panel.getByRole('button', { name: raiseTo('Full') }));

      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_1', 'full', ['ag_a']),
      );
    });

    it('keeps the raise of a Shared account disabled while it has no management group left', async () => {
      renderView({ isAdmin: true, accounts: [shared({ admin_groups: [] })] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      expect(panel.getByRole('button', { name: raiseTo('Full') })).toBeDisabled();
      expect(panel.getByText(t.vendorAccountShareNoGroupsHint)).toBeInTheDocument();
    });

    it('stops sharing only after confirmation, then reads the account as not shared', async () => {
      const { fakeApi } = renderView({ isAdmin: true, accounts: [shared()] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      fireEvent.click(panel.getByRole('button', { name: t.vendorAccountShareStopAction }));
      const dialog = within(await screen.findByRole('dialog'));
      expect(dialog.getByText(t.vendorAccountShareStopConfirm)).toBeInTheDocument();
      expect(fakeApi.setVendorAccountShareLevel).not.toHaveBeenCalled();
      fireEvent.click(dialog.getByRole('button', { name: t.vendorAccountShareStopAction }));

      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_1', 'none'),
      );
      await waitFor(() =>
        expect(within(shareRegion()!).getByLabelText(t.vendorAccountShareLevelLabel)).toHaveValue(
          t.vendorAccountShareLevelNone,
        ),
      );
      // Not shared any more: no management-group editor either.
      expect(groupsRegion()).not.toBeInTheDocument();
    });

    it('lets an owner who is no longer an admin stop sharing, but offers no raise', async () => {
      renderView({ isAdmin: false, accounts: [shared()] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      expect(
        panel.getByRole('button', { name: t.vendorAccountShareStopAction }),
      ).toBeInTheDocument();
      expect(panel.queryByRole('button', { name: raiseTo('Full') })).not.toBeInTheDocument();
    });

    it('lets the owner of a Fully shared account lower it to Shared without a confirmation', async () => {
      const { fakeApi } = renderView({
        isAdmin: true,
        accounts: [shared({ share_level: 'full' })],
      });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      // Nothing higher than full to raise to.
      expect(panel.queryByRole('button', { name: raiseTo('Full') })).not.toBeInTheDocument();
      expect(panel.queryByRole('button', { name: raiseTo('Shared') })).not.toBeInTheDocument();
      expect(
        panel.getByRole('button', { name: t.vendorAccountShareStopAction }),
      ).toBeInTheDocument();
      fireEvent.click(panel.getByRole('button', { name: lowerToShared }));

      await waitFor(() =>
        expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledWith('va_1', 'shared'),
      );
      expect(fakeApi.setVendorAccountShareLevel).toHaveBeenCalledTimes(1);
    });

    it('lets a co-manager of a Fully shared account lower it, and never raise it', async () => {
      renderView({ isAdmin: true, accounts: [managed({ share_level: 'full' })] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      expect(panel.getByRole('button', { name: lowerToShared })).toBeInTheDocument();
      expect(
        panel.getByRole('button', { name: t.vendorAccountShareStopAction }),
      ).toBeInTheDocument();
      expect(panel.queryByRole('button', { name: raiseTo('Full') })).not.toBeInTheDocument();
    });

    it('drops the account from the list when a co-manager stops sharing it (it is no longer theirs)', async () => {
      const setVendorAccountShareLevel = vi.fn(async () =>
        managed({
          share_level: 'none',
          admin_groups: [],
          system_group_id: '',
          system_group_name: '',
        }),
      );
      renderView({
        accounts: [managed({ share_level: 'full' })],
        setVendorAccountShareLevel,
      });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      fireEvent.click(panel.getByRole('button', { name: t.vendorAccountShareStopAction }));
      const dialog = within(await screen.findByRole('dialog'));
      fireEvent.click(dialog.getByRole('button', { name: t.vendorAccountShareStopAction }));

      // Back on the list, and the account is gone: the next read would be a 404.
      expect(
        await screen.findByRole('button', { name: t.vendorAccountCreate }),
      ).toBeInTheDocument();
      expect(screen.queryByText('Work OpenAI')).not.toBeInTheDocument();
    });

    it('shows a co-manager of a Shared account the level read-only, with no way to change it', async () => {
      renderView({ isAdmin: true, accounts: [managed()] });
      await openDetail();
      const panel = within(await screen.findByRole('region', { name: t.vendorAccountShareTitle }));

      expect(panel.getByLabelText(t.vendorAccountShareLevelLabel)).toHaveValue(
        t.vendorAccountShareLevelShared,
      );
      expect(panel.getByText(t.vendorAccountShareReadOnlyNote)).toBeInTheDocument();
      expect(panel.queryByRole('button')).not.toBeInTheDocument();
      expect(panel.queryByLabelText(t.vendorAccountShareTargetLabel)).not.toBeInTheDocument();
    });
  });

  describe(`VendorAccountsView management-group editor [${locale}]`, () => {
    it('lets the owner of a Shared account replace the groups, offering only those under its root', async () => {
      const { fakeApi } = renderView({
        accounts: [shared()],
        candidates: [GROUP_A, GROUP_B, GROUP_OTHER],
      });
      await openDetail();
      await screen.findByRole('region', { name: t.vendorAccountAdminGroupsSectionTitle });

      // Pre-filled with the account's own group; a group under another root is not offered.
      expect(screen.getByText('Group A')).toBeInTheDocument();
      expect(screen.queryByText("Other System's Group")).not.toBeInTheDocument();

      fireEvent.mouseDown(screen.getByLabelText(t.vendorAccountAdminGroupLabel));
      fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));
      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountAdminGroupsSave }));

      await waitFor(() =>
        expect(fakeApi.setVendorAccountAdminGroups).toHaveBeenCalledWith('va_1', ['ag_a', 'ag_b']),
      );
      expect(await screen.findByText(t.vendorAccountAdminGroupsSaved)).toBeInTheDocument();
    });

    it('keeps Save disabled when the last group is removed (at least one is required)', async () => {
      renderView({ accounts: [shared()], candidates: [GROUP_A, GROUP_B] });
      await openDetail();
      const region = within(
        await screen.findByRole('region', { name: t.vendorAccountAdminGroupsSectionTitle }),
      );

      expect(region.getByRole('button', { name: t.vendorAccountAdminGroupsSave })).toBeEnabled();
      fireEvent.click(region.getByTestId('CancelIcon'));
      expect(region.getByRole('button', { name: t.vendorAccountAdminGroupsSave })).toBeDisabled();
    });

    // Spec 6, "Management-group assignment": owner-only at shared, owner + CM at full,
    // never for anyone at none, and never for a member. Whether the viewer is an admin
    // does not matter: the editor follows the owner / co-manager tier and the level.
    it.each([
      ['an admin owner at Shared', () => shared(), true, true],
      ['a plain owner at Shared', () => shared(), false, true],
      ['the owner at Fully shared', () => shared({ share_level: 'full' }), false, true],
      ['a co-manager at Fully shared', () => managed({ share_level: 'full' }), false, true],
      ['a co-manager at Shared', () => managed(), true, false],
      ['the owner of a personal account', () => makeVendorAccount(), false, false],
      ['a member', () => member(), true, false],
    ] as const)('shows the editor to %s: %s', async (_who, makeAccount, isAdmin, visible) => {
      const { fakeApi } = renderView({ accounts: [makeAccount()], isAdmin });
      await openDetail();
      // Wait for the detail view to settle (its panels all mount in one render).
      await waitFor(() =>
        expect(
          screen.queryByText(t.vendorAccountSettingsTitle) ??
            screen.queryByText(t.vendorAccountMemberTitle),
        ).toBeInTheDocument(),
      );

      if (visible) {
        expect(
          await screen.findByRole('region', { name: t.vendorAccountAdminGroupsSectionTitle }),
        ).toBeInTheDocument();
      } else {
        expect(groupsRegion()).not.toBeInTheDocument();
        // Nothing to pick from either, so only the admin owner of a personal account
        // (the raise picker) ever fetches candidates.
        expect(fakeApi.vendorAccountAdminGroupCandidates).not.toHaveBeenCalled();
      }
    });
  });

  describe(`VendorAccountsView credential panels by tier [${locale}]`, () => {
    it('hides the api key and the delete action from a co-manager at Shared, and never sends an api_key', async () => {
      const { fakeApi } = renderView({ accounts: [managed()] });
      await openDetail();
      await screen.findByText(t.vendorAccountSettingsTitle);

      // The credential is the owner's alone at Shared; the backend would answer 403.
      expect(screen.queryByLabelText(t.vendorAccountApiKeyLabel)).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.vendorAccountApiKeyClear }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.vendorAccountActionDelete }),
      ).not.toBeInTheDocument();
      // The owner is named; operate stays: rename, status and prefix.
      expect(screen.getByLabelText(t.vendorAccountSharedBy)).toHaveValue('Olga Owner');

      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Renamed by CM' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
      const [id, body] = fakeApi.updateVendorAccount.mock.calls[0];
      expect(id).toBe('va_1');
      expect(body).toEqual({ name: 'Renamed by CM', status: 'active', model_prefix: '' });
      expect('api_key' in body).toBe(false);
    });

    it('shows a co-manager at Fully shared the api key and the delete action (full is owner-equivalent)', async () => {
      renderView({ accounts: [managed({ share_level: 'full' })] });
      await openDetail();
      await screen.findByText(t.vendorAccountSettingsTitle);

      expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.vendorAccountActionDelete })).toBeInTheDocument();
    });

    it('shows the owner of a shared account the api key and delete, and does not name an owner', async () => {
      renderView({ accounts: [shared()] });
      await openDetail();
      await screen.findByText(t.vendorAccountSettingsTitle);

      expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.vendorAccountActionDelete })).toBeInTheDocument();
      expect(screen.queryByLabelText(t.vendorAccountSharedBy)).not.toBeInTheDocument();
    });

    it('hides the subscription connect panel from a co-manager of a shared subscription, shows it to the owner', async () => {
      const subscription = { ...OPENAI_SUBSCRIPTION, auth_type: 'subscription' } as const;
      const { unmount } = renderView({ accounts: [managed(subscription)] });
      await openDetail();
      await screen.findByText(t.vendorAccountSettingsTitle);
      expect(screen.queryByRole('region', { name: t.vendorConnectTitle })).not.toBeInTheDocument();
      unmount();

      renderView({ accounts: [shared(subscription)] });
      await openDetail();
      expect(await screen.findByRole('region', { name: t.vendorConnectTitle })).toBeInTheDocument();
    });
  });

  describe(`VendorAccountsView co-manager of a shared subscription account [${locale}]`, () => {
    const sharedSubscription = (overrides: Partial<VendorAccount> = {}) =>
      managed({ ...OPENAI_SUBSCRIPTION, models: [], ...overrides });

    it('offers no connection test, models refresh, usage refresh or on-view pull, but still reads the usage', async () => {
      const { fakeApi } = renderView({ accounts: [sharedSubscription()] });
      await openDetail();

      // The usage panel itself stays (a management read); the snapshot is read once...
      expect(await screen.findByRole('region', { name: t.vendorUsageTitle })).toBeInTheDocument();
      await waitFor(() => expect(fakeApi.vendorAccount).toHaveBeenCalledWith('va_oai_sub'));

      // ...but every call that would open the owner's single-use token is not offered
      // (the backend answers 403 for each of them).
      expect(screen.queryByRole('region', { name: t.vendorCheckTitle })).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.vendorCheckAction })).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.vendorModelsRefreshAction }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.vendorUsageRefreshAction }),
      ).not.toBeInTheDocument();
      expect(fakeApi.refreshUsage).not.toHaveBeenCalled();
      expect(fakeApi.testConnection).not.toHaveBeenCalled();
      expect(fakeApi.refreshModels).not.toHaveBeenCalled();
    });

    it('still lists the models of the account and lets the co-manager operate the rest (rename, status, prefix)', async () => {
      renderView({
        accounts: [
          sharedSubscription({
            models: [
              {
                gateway_model: 'chatgpt/gpt-6-luna',
                upstream_model: 'gpt-6-luna',
                api_flavor: 'openai_responses',
                display_name: 'GPT-6 Luna',
              },
            ],
          }),
        ],
      });
      await openDetail();

      expect(await screen.findByText('chatgpt/gpt-6-luna')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.save })).toBeInTheDocument();
    });

    it('gives the owner of the same shared subscription the test, refresh and the on-view pull', async () => {
      const { fakeApi } = renderView({
        accounts: [shared({ ...OPENAI_SUBSCRIPTION })],
      });
      await openDetail();

      expect(await screen.findByRole('button', { name: t.vendorCheckAction })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.vendorModelsRefreshAction })).toBeInTheDocument();
      expect(
        await screen.findByRole('button', { name: t.vendorUsageRefreshAction }),
      ).toBeInTheDocument();
      await waitFor(() => expect(fakeApi.refreshUsage).toHaveBeenCalledWith('va_oai_sub'));
    });

    it('lets a co-manager of a shared API-KEY account test, refresh and pull usage (owner + co-manager operate)', async () => {
      const { fakeApi } = renderView({ accounts: [managed(OPENROUTER_KEY)] });
      await openDetail();

      expect(await screen.findByRole('button', { name: t.vendorCheckAction })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.vendorModelsRefreshAction })).toBeInTheDocument();
      expect(
        await screen.findByRole('button', { name: t.vendorUsageRefreshAction }),
      ).toBeInTheDocument();
      await waitFor(() => expect(fakeApi.refreshUsage).toHaveBeenCalledWith('va_or'));
    });
  });

  describe(`VendorAccountsView member (minimal view) [${locale}]`, () => {
    it('lists the account a member only borrows, like any other row', async () => {
      renderView({ accounts: [member()] });

      const row = (await screen.findByText('Work OpenAI')).closest('tr')!;
      expect(within(row).getByText(t.statusActive)).toBeInTheDocument();
    });

    it('opens a read-only view of name, status, owner and models by gateway name, and nothing of the management', async () => {
      const { fakeApi } = renderView({ accounts: [member()], isAdmin: true });
      await openDetail();

      await screen.findByRole('region', { name: t.vendorAccountMemberTitle });
      expect(screen.getByText(t.vendorAccountMemberNote)).toBeInTheDocument();
      // Name, status and owner are shown, but not editable.
      expect(screen.getByLabelText(t.vendorAccountNameLabel)).toHaveValue('Work OpenAI');
      expect(screen.getByLabelText(t.vendorAccountNameLabel)).toHaveAttribute('readonly');
      expect(screen.getByLabelText(t.vendorAccountStatusLabel)).toHaveValue(t.statusActive);
      expect(screen.getByLabelText(t.vendorAccountSharedBy)).toHaveValue('Olga Owner');
      // The model list is by the name a client requests (and the vendor's display name).
      expect(screen.getByText('team/gpt-6')).toBeInTheDocument();
      expect(screen.getByText('GPT-6')).toBeInTheDocument();

      // No settings form, no credential, no delete, no operate calls.
      expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.vendorAccountApiKeyLabel)).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.vendorAccountActionDelete }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.vendorModelsRefreshAction }),
      ).not.toBeInTheDocument();
      expect(screen.queryByRole('region', { name: t.vendorCheckTitle })).not.toBeInTheDocument();
      expect(screen.queryByRole('region', { name: t.vendorConnectTitle })).not.toBeInTheDocument();
      // No share area, no management-group editor, no usage: the member DTO withholds them.
      expect(shareRegion()).not.toBeInTheDocument();
      expect(groupsRegion()).not.toBeInTheDocument();
      expect(screen.queryByRole('region', { name: t.vendorUsageTitle })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.vendorAccountShareLevelLabel)).not.toBeInTheDocument();
      // ...and none of them was fetched on the member's behalf.
      expect(fakeApi.vendorAccount).not.toHaveBeenCalled();
      expect(fakeApi.refreshUsage).not.toHaveBeenCalled();
      expect(fakeApi.vendorAccountAdminGroupCandidates).not.toHaveBeenCalled();
    });

    it('never renders the zero timestamp the member DTO carries, as a date or as text', async () => {
      const { container } = renderView({ accounts: [member()] });
      await openDetail();
      await screen.findByRole('region', { name: t.vendorAccountMemberTitle });

      expect(container.textContent).not.toContain('0001');
      expect(container.textContent).not.toContain('1.1.1');
      expect(container.textContent).not.toContain('1/1/1');
    });

    it('shows no empty base-URL field for a configurable-root vendor (the member DTO withholds it)', async () => {
      renderView({ accounts: [member({ ...OPENROUTER_KEY, base_url: '' })] });
      await openDetail();
      await screen.findByRole('region', { name: t.vendorAccountMemberTitle });

      expect(screen.queryByLabelText(t.vendorAccountBaseUrlLabel)).not.toBeInTheDocument();
    });
  });
}
