// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VendorAccountsView } from './VendorAccountsView';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import { PortalApiError } from '../api';
import type { CreateVendorAccountRequest, UpdateVendorAccountRequest, VendorAccount } from '../api';
import type { PortalApi } from './shared/types';

function makeVendorAccount(overrides: Partial<VendorAccount> = {}): VendorAccount {
  return {
    id: 'va_1',
    vendor: 'openai',
    auth_type: 'api_key',
    name: 'Work OpenAI',
    status: 'active',
    api_key_set: true,
    subscription_connected: false,
    models: [],
    created_at: '2026-09-01T12:00:00Z',
    updated_at: '2026-09-01T12:00:00Z',
    ...overrides,
  };
}

afterEach(cleanup);

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderView(
    opts: {
      accounts?: VendorAccount[];
      createVendorAccount?: PortalApi['createVendorAccount'];
      updateVendorAccount?: PortalApi['updateVendorAccount'];
      deleteVendorAccount?: PortalApi['deleteVendorAccount'];
    } = {},
  ) {
    const accounts = opts.accounts ?? [makeVendorAccount()];
    const fakeApi = {
      vendorAccounts: vi.fn(async () => ({ data: accounts })),
      createVendorAccount: vi.fn<PortalApi['createVendorAccount']>(
        opts.createVendorAccount ??
          (async (body: CreateVendorAccountRequest) =>
            makeVendorAccount({
              id: 'va_created',
              vendor: body.vendor as VendorAccount['vendor'],
              name: body.name,
              api_key_set: Boolean(body.api_key),
            })),
      ),
      updateVendorAccount: vi.fn<PortalApi['updateVendorAccount']>(
        opts.updateVendorAccount ??
          (async (id: string, body: UpdateVendorAccountRequest) =>
            makeVendorAccount({
              id,
              name: body.name ?? accounts[0].name,
              status: (body.status as VendorAccount['status']) ?? accounts[0].status,
              api_key_set: body.api_key === '' ? false : accounts[0].api_key_set || !!body.api_key,
            })),
      ),
      deleteVendorAccount: vi.fn<PortalApi['deleteVendorAccount']>(
        opts.deleteVendorAccount ?? (async () => ({ ok: true })),
      ),
    };
    const view = render(
      <ToastProvider>
        <VendorAccountsView t={t} api={fakeApi} />
      </ToastProvider>,
    );
    return { fakeApi, ...view };
  }

  describe(`VendorAccountsView list [${locale}]`, () => {
    it('renders vendor, authentication, name, status and credential columns', async () => {
      renderView({
        accounts: [
          makeVendorAccount({
            vendor: 'anthropic',
            name: 'Team Claude',
            status: 'disabled',
            api_key_set: false,
          }),
        ],
      });

      await screen.findByText('Team Claude');
      const row = screen.getByText('Team Claude').closest('tr')!;
      expect(within(row).getByText(t.vendorAnthropic)).toBeInTheDocument();
      expect(within(row).getByText(t.vendorAuthApiKey)).toBeInTheDocument();
      expect(within(row).getByText(t.statusDisabled)).toBeInTheDocument();
      expect(within(row).getByText(t.vendorAccountCredentialMissing)).toBeInTheDocument();
    });

    it('shows a stored credential as stored and never renders any secret', async () => {
      renderView();

      await screen.findByText('Work OpenAI');
      const row = screen.getByText('Work OpenAI').closest('tr')!;
      expect(within(row).getByText(t.vendorAccountCredentialSet)).toBeInTheDocument();
    });

    it('labels a system-managed needs_reconnect account instead of showing it as active', async () => {
      renderView({
        accounts: [makeVendorAccount({ name: 'Sub', status: 'needs_reconnect' })],
      });

      await screen.findByText('Sub');
      const row = screen.getByText('Sub').closest('tr')!;
      expect(within(row).getByText(t.vendorAccountStatusNeedsReconnect)).toBeInTheDocument();
      expect(within(row).queryByText(t.statusActive)).not.toBeInTheDocument();
    });

    it('offers the create action to every user (no role gate)', async () => {
      renderView();
      expect(
        await screen.findByRole('button', { name: t.vendorAccountCreate }),
      ).toBeInTheDocument();
    });
  });

  describe(`VendorAccountsView create form [${locale}]`, () => {
    async function openCreate() {
      fireEvent.click(await screen.findByRole('button', { name: t.vendorAccountCreate }));
      await screen.findByLabelText(t.vendorAccountApiKeyLabel);
    }

    it('creates an api_key account, sends the key once, and never shows it', async () => {
      const { fakeApi, container } = renderView({ accounts: [] });
      await openCreate();

      // Write-only secret: a masked, non-autofilled input.
      const keyInput = screen.getByLabelText(t.vendorAccountApiKeyLabel);
      expect(keyInput).toHaveAttribute('type', 'password');
      expect(keyInput).toHaveAttribute('autocomplete', 'new-password');

      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Work' },
      });
      fireEvent.change(keyInput, { target: { value: 'sk-test-123' } });
      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountCreate }));

      await waitFor(() => expect(fakeApi.createVendorAccount).toHaveBeenCalledTimes(1));
      // M2 UI scope: vendor + the api-key path only; the account is api_key.
      expect(fakeApi.createVendorAccount).toHaveBeenCalledWith({
        vendor: 'openai',
        auth_type: 'api_key',
        name: 'Work',
        api_key: 'sk-test-123',
      });

      // Back on the list with the new account; the key is nowhere in the DOM.
      expect(await screen.findByText('Work')).toBeInTheDocument();
      expect(container.innerHTML).not.toContain('sk-test-123');
      expect(screen.queryByDisplayValue('sk-test-123')).not.toBeInTheDocument();
    });

    it('submits the chosen vendor', async () => {
      const { fakeApi } = renderView({ accounts: [] });
      await openCreate();

      fireEvent.mouseDown(screen.getByLabelText(t.vendorAccountVendorLabel));
      fireEvent.click(await screen.findByRole('option', { name: t.vendorAnthropic }));
      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Claude' },
      });
      fireEvent.change(screen.getByLabelText(t.vendorAccountApiKeyLabel), {
        target: { value: 'sk-ant-1' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountCreate }));

      await waitFor(() => expect(fakeApi.createVendorAccount).toHaveBeenCalledTimes(1));
      expect(fakeApi.createVendorAccount.mock.calls[0][0]).toMatchObject({
        vendor: 'anthropic',
        auth_type: 'api_key',
      });
    });

    it('offers only the api-key path: no subscription auth type, no connect button', async () => {
      renderView({ accounts: [] });
      await openCreate();

      expect(screen.queryByText(t.vendorAuthSubscription)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.vendorAccountAuthTypeLabel)).not.toBeInTheDocument();
    });

    it('does not keep a typed key when the form is cancelled and reopened', async () => {
      renderView({ accounts: [] });
      await openCreate();
      fireEvent.change(screen.getByLabelText(t.vendorAccountApiKeyLabel), {
        target: { value: 'sk-leftover' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.cancel }));
      await openCreate();

      expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).toHaveValue('');
    });

    it('surfaces a refused create as a localized toast and stays on the form', async () => {
      const createVendorAccount = vi.fn(async () => {
        throw new PortalApiError(
          400,
          'vendor_account.api_key_key_required',
          'an encryption key is required',
        );
      });
      renderView({ accounts: [], createVendorAccount });
      await openCreate();

      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Work' },
      });
      fireEvent.change(screen.getByLabelText(t.vendorAccountApiKeyLabel), {
        target: { value: 'sk-test-123' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountCreate }));

      expect(
        await screen.findByText(
          `vendor_account.api_key_key_required: ${t.errorVendorAccountApiKeyKeyRequired}`,
        ),
      ).toBeInTheDocument();
      expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).toBeInTheDocument();
    });
  });

  describe(`VendorAccountsView detail [${locale}]`, () => {
    async function openDetail() {
      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.vendorAccountSettingsTitle);
    }

    it('marks a stored key with the set placeholder and never echoes it', async () => {
      renderView();
      await openDetail();

      const keyInput = screen.getByLabelText(t.vendorAccountApiKeyLabel);
      expect(keyInput).toHaveAttribute('placeholder', t.vendorAccountApiKeySetPlaceholder);
      expect(keyInput).toHaveValue('');
      expect(screen.getByRole('button', { name: t.vendorAccountApiKeyClear })).toBeInTheDocument();
    });

    it('shows no set placeholder and no clear button when no key is stored', async () => {
      renderView({ accounts: [makeVendorAccount({ api_key_set: false })] });
      await openDetail();

      expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).not.toHaveAttribute('placeholder');
      expect(
        screen.queryByRole('button', { name: t.vendorAccountApiKeyClear }),
      ).not.toBeInTheDocument();
    });

    it('shows vendor and authentication as read-only (immutable) fields', async () => {
      renderView();
      await openDetail();

      expect(screen.getByLabelText(t.vendorAccountVendorLabel)).toHaveValue(t.vendorOpenAI);
      expect(screen.getByLabelText(t.vendorAccountAuthTypeLabel)).toHaveValue(t.vendorAuthApiKey);
    });

    it('keeps the stored key when saving a rename (api_key omitted from the PATCH)', async () => {
      const { fakeApi } = renderView();
      await openDetail();

      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Renamed' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
      const [id, body] = fakeApi.updateVendorAccount.mock.calls[0];
      expect(id).toBe('va_1');
      expect(body).toEqual({ name: 'Renamed', status: 'active' });
      expect('api_key' in body).toBe(false);
    });

    it('sends a changed status', async () => {
      const { fakeApi } = renderView();
      await openDetail();

      fireEvent.mouseDown(screen.getByLabelText(t.vendorAccountStatusLabel));
      fireEvent.click(await screen.findByRole('option', { name: t.statusDisabled }));
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
      expect(fakeApi.updateVendorAccount.mock.calls[0][1]).toEqual({
        name: 'Work OpenAI',
        status: 'disabled',
      });
    });

    it('replaces the key when a new one is typed, then resets the field', async () => {
      const { fakeApi, container } = renderView();
      await openDetail();

      fireEvent.change(screen.getByLabelText(t.vendorAccountApiKeyLabel), {
        target: { value: 'sk-rotated' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
      expect(fakeApi.updateVendorAccount.mock.calls[0][1]).toMatchObject({
        api_key: 'sk-rotated',
      });
      await waitFor(() =>
        expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).toHaveValue(''),
      );
      expect(container.innerHTML).not.toContain('sk-rotated');
    });

    it('clears the key with the exact empty string once Save commits the Clear', async () => {
      const { fakeApi } = renderView();
      await openDetail();

      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountApiKeyClear }));
      // Pending clear: the set placeholder and the clear button are gone, but
      // nothing has been sent yet.
      expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).not.toHaveAttribute('placeholder');
      expect(
        screen.queryByRole('button', { name: t.vendorAccountApiKeyClear }),
      ).not.toBeInTheDocument();
      expect(fakeApi.updateVendorAccount).not.toHaveBeenCalled();

      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
      expect(fakeApi.updateVendorAccount.mock.calls[0][1]).toMatchObject({ api_key: '' });
    });

    it('lets a system-managed needs_reconnect status ride along unchanged', async () => {
      const account = makeVendorAccount({ status: 'needs_reconnect' });
      const { fakeApi } = renderView({ accounts: [account] });
      await openDetail();

      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
      expect(fakeApi.updateVendorAccount.mock.calls[0][1].status).toBe('needs_reconnect');
    });

    it('surfaces a refused save as a localized toast', async () => {
      const updateVendorAccount = vi.fn(async () => {
        throw new PortalApiError(400, 'vendor_account.api_key_invalid', 'raw server text');
      });
      renderView({ updateVendorAccount });
      await openDetail();

      fireEvent.change(screen.getByLabelText(t.vendorAccountApiKeyLabel), {
        target: { value: '   ' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      expect(
        await screen.findByText(
          `vendor_account.api_key_invalid: ${t.errorVendorAccountApiKeyInvalid}`,
        ),
      ).toBeInTheDocument();
    });

    it('deletes the account after confirmation and returns to the list', async () => {
      const { fakeApi } = renderView();
      await openDetail();

      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountActionDelete }));
      const dialog = within(await screen.findByRole('dialog'));
      expect(dialog.getByText(t.vendorAccountDeleteConfirm)).toBeInTheDocument();
      expect(fakeApi.deleteVendorAccount).not.toHaveBeenCalled();
      fireEvent.click(dialog.getByRole('button', { name: t.vendorAccountActionDelete }));

      await waitFor(() => expect(fakeApi.deleteVendorAccount).toHaveBeenCalledWith('va_1'));
      // Back on the list, and the deleted row is gone.
      expect(
        await screen.findByRole('button', { name: t.vendorAccountCreate }),
      ).toBeInTheDocument();
      expect(screen.queryByText('Work OpenAI')).not.toBeInTheDocument();
    });
  });
}
