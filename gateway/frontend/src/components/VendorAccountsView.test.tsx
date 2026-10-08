// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VendorAccountsView } from './VendorAccountsView';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import { PortalApiError } from '../api';
import { DEVICE_POLL_INTERVAL_MS, DEVICE_POLL_TIMEOUT_MS } from './VendorDeviceConnect';
import type {
  CreateVendorAccountRequest,
  UpdateVendorAccountRequest,
  VendorAccount,
  VendorAccountUsage,
  VendorConnectionCheck,
} from '../api';
import type { MessageKey, PortalApi } from './shared/types';

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

const AUTHORIZE_URL = 'https://claude.ai/oauth/authorize?client_id=x&state=s1';
const DEVICE_URL = 'https://auth.example/codex/device';
const DEVICE_CODE = 'ABCD-EFGH';

// A subscription account that has not been connected yet.
const SUBSCRIPTION: Partial<VendorAccount> = {
  id: 'va_sub',
  vendor: 'anthropic',
  auth_type: 'subscription',
  name: 'Team Claude Max',
  api_key_set: false,
  subscription_connected: false,
};

// A rate-limit snapshot as the single-account GET returns it. The 5-hour window
// resets in 2 h 14 min (plus 30 s of slack, so the countdown still reads 14 min
// after the test's own latency); the weekly one in 3 d 4 h.
function makeUsage(overrides: Partial<VendorAccountUsage> = {}): VendorAccountUsage {
  const now = Date.now();
  return {
    five_hour_pct: 42,
    five_hour_reset_at: new Date(now + (2 * 60 + 14) * 60_000 + 30_000).toISOString(),
    weekly_pct: 7,
    weekly_reset_at: new Date(now + (3 * 24 + 4) * 3_600_000 + 30_000).toISOString(),
    credit_balance: '',
    updated_at: new Date(now - 5 * 60_000).toISOString(),
    ...overrides,
  };
}

// The credential-check verdict as POST .../check answers it. The detail is the
// token-free ENGLISH status phrase the backend sends whatever the portal locale.
function makeCheck(overrides: Partial<VendorConnectionCheck> = {}): VendorConnectionCheck {
  return {
    status: 'valid',
    detail: 'the vendor accepted the credential',
    checked_at: '2026-10-08T10:00:00Z',
    ...overrides,
  };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderView(
    opts: {
      accounts?: VendorAccount[];
      createVendorAccount?: PortalApi['createVendorAccount'];
      updateVendorAccount?: PortalApi['updateVendorAccount'];
      deleteVendorAccount?: PortalApi['deleteVendorAccount'];
      connectVendorAccountImport?: PortalApi['connectVendorAccountImport'];
      beginVendorAccountConnect?: PortalApi['beginVendorAccountConnect'];
      completeVendorAccountConnect?: PortalApi['completeVendorAccountConnect'];
      beginVendorAccountDeviceConnect?: PortalApi['beginVendorAccountDeviceConnect'];
      pollVendorAccountDeviceConnect?: PortalApi['pollVendorAccountDeviceConnect'];
      vendorAccount?: PortalApi['vendorAccount'];
      testConnection?: PortalApi['testConnection'];
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
              auth_type: body.auth_type as VendorAccount['auth_type'],
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
      // The connect calls answer the credential-free DTO, connected and active
      // (what the backend's persistVendorTokens writes).
      connectVendorAccountImport: vi.fn<PortalApi['connectVendorAccountImport']>(
        opts.connectVendorAccountImport ??
          (async (id: string) =>
            makeVendorAccount({
              ...SUBSCRIPTION,
              id,
              api_key_set: false,
              subscription_connected: true,
            })),
      ),
      beginVendorAccountConnect: vi.fn<PortalApi['beginVendorAccountConnect']>(
        opts.beginVendorAccountConnect ?? (async () => ({ authorize_url: AUTHORIZE_URL })),
      ),
      completeVendorAccountConnect: vi.fn<PortalApi['completeVendorAccountConnect']>(
        opts.completeVendorAccountConnect ??
          (async (id: string) =>
            makeVendorAccount({
              ...SUBSCRIPTION,
              id,
              api_key_set: false,
              subscription_connected: true,
            })),
      ),
      // Device code: begin shows a pairing code, the poll is pending until a test
      // says otherwise, and the post-connect re-read answers the account as the
      // backend stores it after a connect (connected + active).
      beginVendorAccountDeviceConnect: vi.fn<PortalApi['beginVendorAccountDeviceConnect']>(
        opts.beginVendorAccountDeviceConnect ??
          (async () => ({ user_code: DEVICE_CODE, verification_url: DEVICE_URL })),
      ),
      pollVendorAccountDeviceConnect: vi.fn<PortalApi['pollVendorAccountDeviceConnect']>(
        opts.pollVendorAccountDeviceConnect ?? (async () => ({ connected: false })),
      ),
      vendorAccount: vi.fn<PortalApi['vendorAccount']>(
        opts.vendorAccount ??
          (async (id: string) =>
            makeVendorAccount({
              ...(accounts.find((a) => a.id === id) ?? accounts[0]),
              api_key_set: false,
              subscription_connected: true,
              status: 'active',
            })),
      ),
      testConnection: vi.fn<PortalApi['testConnection']>(
        opts.testConnection ?? (async () => makeCheck()),
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

    it('offers an authentication choice that defaults to api key, with the key field', async () => {
      renderView({ accounts: [] });
      await openCreate();

      expect(screen.getByLabelText(t.vendorAccountAuthTypeLabel)).toHaveTextContent(
        t.vendorAuthApiKey,
      );
      expect(document.getElementById('vendor-account-api-key')).toBeInTheDocument();
      expect(screen.queryByText(t.vendorAccountSubscriptionCreateNote)).not.toBeInTheDocument();
    });

    async function chooseAuthType(optionName: string) {
      fireEvent.mouseDown(screen.getByLabelText(t.vendorAccountAuthTypeLabel));
      fireEvent.click(await screen.findByRole('option', { name: optionName }));
      // The menu closes with a transition; wait it out so the select's label is
      // unambiguous again for the next query.
      await waitFor(() => expect(screen.queryByRole('listbox')).not.toBeInTheDocument());
    }
    const chooseSubscription = () => chooseAuthType(t.vendorAuthSubscription);

    it('creates a subscription account with no key field, then opens its detail to connect', async () => {
      const { fakeApi } = renderView({ accounts: [] });
      await openCreate();

      await chooseSubscription();
      // No key field for a subscription account, only the note that explains why.
      expect(document.getElementById('vendor-account-api-key')).not.toBeInTheDocument();
      expect(screen.getByText(t.vendorAccountSubscriptionCreateNote)).toBeInTheDocument();

      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Team Claude Max' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountCreate }));

      await waitFor(() => expect(fakeApi.createVendorAccount).toHaveBeenCalledTimes(1));
      // A disconnected subscription account: no api_key at all (not even "").
      expect(fakeApi.createVendorAccount).toHaveBeenCalledWith({
        vendor: 'openai',
        auth_type: 'subscription',
        name: 'Team Claude Max',
      });
      expect('api_key' in fakeApi.createVendorAccount.mock.calls[0][0]).toBe(false);

      // Straight to the detail view of the new account, not yet connected.
      expect(await screen.findByText(t.vendorConnectTitle)).toBeInTheDocument();
      expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();
      expect(screen.getByLabelText(t.vendorAccountNameLabel)).toHaveValue('Team Claude Max');
    });

    it('does not carry a typed key into a subscription account and back', async () => {
      renderView({ accounts: [] });
      await openCreate();
      fireEvent.change(document.getElementById('vendor-account-api-key')!, {
        target: { value: 'sk-leftover' },
      });

      await chooseSubscription();
      await chooseAuthType(t.vendorAuthApiKey);

      expect(document.getElementById('vendor-account-api-key')).toHaveValue('');
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

  describe(`VendorAccountsView subscription connect [${locale}]`, () => {
    function renderSubscription(
      overrides: Partial<VendorAccount> = {},
      opts: Parameters<typeof renderView>[0] = {},
    ) {
      return renderView({
        ...opts,
        accounts: [makeVendorAccount({ ...SUBSCRIPTION, ...overrides })],
      });
    }

    async function openDetail() {
      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.vendorAccountSettingsTitle);
    }

    const refusal = (code: string) => async () => {
      throw new PortalApiError(400, code, 'raw server text');
    };

    describe('connection state', () => {
      it('offers no connect panel and keeps the key field on an api_key account', async () => {
        renderView();
        await openDetail();

        expect(screen.queryByText(t.vendorConnectTitle)).not.toBeInTheDocument();
        expect(screen.getByLabelText(t.vendorAccountApiKeyLabel)).toBeInTheDocument();
      });

      it('shows a disconnected subscription as not connected, with both methods and no key field', async () => {
        renderSubscription();
        await openDetail();

        expect(screen.getByText(t.vendorConnectTitle)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toHaveAttribute(
          'data-status',
          'standby',
        );
        expect(screen.queryByText(t.vendorConnectReconnectNote)).not.toBeInTheDocument();
        // Method 1: browser sign-in; method 2: token import.
        expect(screen.getByRole('button', { name: t.vendorConnectBeginAction })).toBeEnabled();
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toBeInTheDocument();
        // A subscription account has no api key to rotate.
        expect(document.getElementById('vendor-account-detail-api-key')).not.toBeInTheDocument();
        expect(screen.getByLabelText(t.vendorAccountAuthTypeLabel)).toHaveValue(
          t.vendorAuthSubscription,
        );
      });

      it('shows a connected subscription as connected and says that connecting again replaces it', async () => {
        renderSubscription({ subscription_connected: true });
        await openDetail();

        expect(screen.getByText(t.vendorConnectStatusConnected)).toHaveAttribute(
          'data-status',
          'active',
        );
        expect(screen.queryByText(t.vendorConnectStatusNotConnected)).not.toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectReconnectNote)).toBeInTheDocument();
      });

      it('flags a connected subscription the gateway can no longer renew', async () => {
        renderSubscription({ subscription_connected: true, status: 'needs_reconnect' });
        await openDetail();

        const chip = screen
          .getAllByText(t.vendorAccountStatusNeedsReconnect)
          .find((el) => el.getAttribute('data-status') === 'watch');
        expect(chip).toBeDefined();
        expect(screen.getByText(t.vendorConnectNeedsReconnectNote)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorConnectReconnectNote)).not.toBeInTheDocument();
      });
    });

    describe('token import', () => {
      const ACCESS = 'at-secret-access';
      const REFRESH = 'rt-secret-refresh';

      function fillAccess(value = ACCESS) {
        fireEvent.change(screen.getByLabelText(t.vendorConnectAccessTokenLabel), {
          target: { value },
        });
      }

      it('masks the token fields and keeps the import disabled until an access token is typed', async () => {
        renderSubscription();
        await openDetail();

        for (const label of [t.vendorConnectAccessTokenLabel, t.vendorConnectRefreshTokenLabel]) {
          const input = screen.getByLabelText(label);
          expect(input).toHaveAttribute('type', 'password');
          expect(input).toHaveAttribute('autocomplete', 'new-password');
          expect(input).toHaveValue('');
        }
        const importButton = screen.getByRole('button', { name: t.vendorConnectImportAction });
        expect(importButton).toBeDisabled();
        fillAccess();
        expect(importButton).toBeEnabled();
      });

      it('sends the tokens and expiry, shows connected, and never renders a token', async () => {
        const { fakeApi, container } = renderSubscription();
        await openDetail();

        fillAccess();
        fireEvent.change(screen.getByLabelText(t.vendorConnectRefreshTokenLabel), {
          target: { value: `  ${REFRESH}  ` },
        });
        fireEvent.change(screen.getByLabelText(t.vendorConnectExpiresAtLabel), {
          target: { value: '2026-10-08T10:30' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));

        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
        // The local wall-clock time of the field goes out as an RFC 3339 instant.
        expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledWith('va_sub', {
          access_token: ACCESS,
          refresh_token: REFRESH,
          expires_at: new Date('2026-10-08T10:30').toISOString(),
        });

        // Connected now, and the success toast says so.
        expect(await screen.findByText(t.vendorConnectStatusConnected)).toHaveAttribute(
          'data-status',
          'active',
        );
        expect(screen.getByText(t.vendorConnectSuccess)).toBeInTheDocument();
        // Write-only: the inputs are emptied and no token is anywhere in the DOM.
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue('');
        expect(screen.getByLabelText(t.vendorConnectRefreshTokenLabel)).toHaveValue('');
        expect(screen.getByLabelText(t.vendorConnectExpiresAtLabel)).toHaveValue('');
        expect(container.innerHTML).not.toContain(ACCESS);
        expect(container.innerHTML).not.toContain(REFRESH);
        expect(screen.queryByDisplayValue(ACCESS)).not.toBeInTheDocument();
      });

      it('sends only the access token when the optional fields are left empty', async () => {
        const { fakeApi } = renderSubscription();
        await openDetail();

        fillAccess();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));

        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
        const body = fakeApi.connectVendorAccountImport.mock.calls[0][1];
        expect(body).toEqual({ access_token: ACCESS });
        expect('refresh_token' in body).toBe(false);
        expect('expires_at' in body).toBe(false);
      });

      it('lists the connected account as stored after an import', async () => {
        renderSubscription();
        await openDetail();
        fillAccess();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));
        await screen.findByText(t.vendorConnectStatusConnected);

        fireEvent.click(screen.getByRole('button', { name: t.providers }));
        const row = (await screen.findByText('Team Claude Max')).closest('tr')!;
        expect(within(row).getByText(t.vendorAccountCredentialSet)).toBeInTheDocument();
      });

      it('re-seeds the status select from the connected account but keeps an unsaved rename', async () => {
        renderSubscription({ status: 'needs_reconnect', subscription_connected: true });
        await openDetail();
        fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
          target: { value: 'Renamed, not saved' },
        });
        fillAccess();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));

        // The import answered active (a connect clears needs_reconnect).
        await screen.findByText(t.vendorConnectStatusConnected);
        expect(screen.getByLabelText(t.vendorAccountStatusLabel)).toHaveTextContent(t.statusActive);
        expect(screen.getByLabelText(t.vendorAccountNameLabel)).toHaveValue('Renamed, not saved');
      });

      // A connect is async: the user can leave the detail view while it is still
      // in flight. When it then completes it must refresh the list, but never
      // re-open its own account over wherever the user went (a later Save would
      // PATCH the wrong account's name and status).
      describe('when the connect resolves after the user navigated away', () => {
        function startSlowImport() {
          let resolveImport!: (account: VendorAccount) => void;
          const pending = new Promise<VendorAccount>((resolve) => {
            resolveImport = resolve;
          });
          const view = renderView({
            accounts: [
              makeVendorAccount({ ...SUBSCRIPTION }),
              makeVendorAccount({ id: 'va_other', name: 'Other OpenAI', status: 'disabled' }),
            ],
            connectVendorAccountImport: () => pending,
          });
          const connected = () =>
            makeVendorAccount({ ...SUBSCRIPTION, status: 'active', subscription_connected: true });
          return { ...view, resolveImport, connected };
        }

        async function openRow(name: string) {
          const row = (await screen.findByText(name)).closest('tr')!;
          fireEvent.click(within(row).getByRole('button', { name: t.modelDetailsAction }));
          await screen.findByText(t.vendorAccountSettingsTitle);
        }

        async function beginImportThenLeave() {
          const started = startSlowImport();
          await openRow('Team Claude Max');
          fillAccess();
          fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));
          await waitFor(() =>
            expect(started.fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1),
          );
          fireEvent.click(screen.getByRole('button', { name: t.providers }));
          return started;
        }

        it('stays on the list and still refreshes the account in it', async () => {
          const { resolveImport, connected } = await beginImportThenLeave();
          await screen.findByText('Other OpenAI');

          await act(async () => resolveImport(connected()));
          await screen.findByText(t.vendorConnectSuccess);

          expect(screen.queryByText(t.vendorAccountSettingsTitle)).not.toBeInTheDocument();
          const row = screen.getByText('Team Claude Max').closest('tr')!;
          expect(within(row).getByText(t.vendorAccountCredentialSet)).toBeInTheDocument();
        });

        it("does not re-seed another account's detail view, so a later Save patches the right account", async () => {
          const { fakeApi, resolveImport, connected } = await beginImportThenLeave();
          await openRow('Other OpenAI');
          expect(screen.getByLabelText(t.vendorAccountStatusLabel)).toHaveTextContent(
            t.statusDisabled,
          );

          await act(async () => resolveImport(connected()));
          await screen.findByText(t.vendorConnectSuccess);

          // Still the other account: its own name and status, not the connected one's.
          expect(screen.getByLabelText(t.vendorAccountNameLabel)).toHaveValue('Other OpenAI');
          expect(screen.getByLabelText(t.vendorAccountStatusLabel)).toHaveTextContent(
            t.statusDisabled,
          );
          fireEvent.click(screen.getByRole('button', { name: t.save }));
          await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalledTimes(1));
          expect(fakeApi.updateVendorAccount.mock.calls[0][0]).toBe('va_other');

          // ... while the connected account was still refreshed in the list.
          fireEvent.click(screen.getByRole('button', { name: t.providers }));
          const row = (await screen.findByText('Team Claude Max')).closest('tr')!;
          expect(within(row).getByText(t.vendorAccountCredentialSet)).toBeInTheDocument();
        });
      });

      it('shows a refused import as a localized toast and keeps what was typed', async () => {
        const { fakeApi } = renderSubscription(
          {},
          {
            connectVendorAccountImport: refusal('vendor_account.connect_key_required'),
          },
        );
        await openDetail();

        fillAccess();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));

        expect(
          await screen.findByText(
            `vendor_account.connect_key_required: ${t.errorVendorAccountConnectKeyRequired}`,
          ),
        ).toBeInTheDocument();
        expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1);
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue(ACCESS);
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();
      });
    });

    // The file picker beside the manual paste: the credential file is read and
    // parsed IN THE BROWSER (parseCredentialFile), the right fields are filled in
    // and the same import is submitted. The raw file never reaches the API.
    describe('file import', () => {
      // base64url of a JSON object: one segment of a JWT.
      const b64url = (value: unknown) =>
        btoa(JSON.stringify(value)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
      const EXP_SECONDS = 1790000000;
      const EXP_RFC3339 = '2026-09-21T14:13:20.000Z';
      const CODEX_ACCESS = `${b64url({ alg: 'none' })}.${b64url({ exp: EXP_SECONDS })}.sig`;
      const CODEX_REFRESH = 'rt-codex-refresh';
      // Fields of the raw file that must never reach the API.
      const ID_TOKEN = 'idtoken-never-sent';
      const ACCOUNT_ID = 'acct-never-sent';

      const codexFile = (overrides: Record<string, unknown> = {}) => ({
        OPENAI_API_KEY: null,
        tokens: {
          id_token: ID_TOKEN,
          access_token: CODEX_ACCESS,
          refresh_token: CODEX_REFRESH,
          account_id: ACCOUNT_ID,
          ...overrides,
        },
        last_refresh: '2026-09-11T10:00:00Z',
      });

      const CLAUDE_ACCESS = 'sk-ant-oat01-claude-access';
      const CLAUDE_REFRESH = 'sk-ant-ort01-claude-refresh';
      const claudeFile = (overrides: Record<string, unknown> = {}) => ({
        claudeAiOauth: {
          accessToken: CLAUDE_ACCESS,
          refreshToken: CLAUDE_REFRESH,
          expiresAt: EXP_SECONDS * 1000,
          scopes: ['user:inference'],
          subscriptionType: 'max',
          ...overrides,
        },
      });

      const fileOf = (name: string, content: unknown) =>
        new File([typeof content === 'string' ? content : JSON.stringify(content)], name, {
          type: 'application/json',
        });

      const picker = () => screen.getByLabelText(t.vendorConnectFileAction);
      function upload(file: File) {
        fireEvent.change(picker(), { target: { files: [file] } });
      }
      it('offers the picker beside the manual paste, with a note that nothing is uploaded', async () => {
        renderSubscription();
        await openDetail();

        expect(screen.getByRole('button', { name: t.vendorConnectFileAction })).toBeEnabled();
        expect(picker()).toHaveAttribute('type', 'file');
        expect(screen.getByText(t.vendorConnectFileNote)).toBeInTheDocument();
        // The manual fields stay as the fallback.
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toBeInTheDocument();
      });

      it('imports a Codex auth.json with the access token, never the id_token or the raw file', async () => {
        const { fakeApi, container } = renderSubscription({ vendor: 'openai' });
        await openDetail();

        upload(fileOf('auth.json', codexFile()));

        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
        expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledWith('va_sub', {
          access_token: CODEX_ACCESS,
          refresh_token: CODEX_REFRESH,
          // The access token's own exp claim, as an RFC 3339 instant.
          expires_at: EXP_RFC3339,
        });
        // Only those three fields: nothing else of the raw file is submitted.
        const sent = JSON.stringify(fakeApi.connectVendorAccountImport.mock.calls);
        for (const secret of [ID_TOKEN, ACCOUNT_ID, 'OPENAI_API_KEY', 'last_refresh', 'tokens']) {
          expect(sent).not.toContain(secret);
        }

        // Connected, success toast, and the write-only fields emptied again.
        expect(await screen.findByText(t.vendorConnectStatusConnected)).toHaveAttribute(
          'data-status',
          'active',
        );
        expect(screen.getByText(t.vendorConnectSuccess)).toBeInTheDocument();
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue('');
        expect(screen.getByLabelText(t.vendorConnectRefreshTokenLabel)).toHaveValue('');
        expect(screen.getByLabelText(t.vendorConnectExpiresAtLabel)).toHaveValue('');
        expect(container.innerHTML).not.toContain(CODEX_ACCESS);
        expect(container.innerHTML).not.toContain(CODEX_REFRESH);
      });

      it('imports a Claude Code .credentials.json with its millisecond expiry', async () => {
        const { fakeApi } = renderSubscription();
        await openDetail();

        upload(fileOf('.credentials.json', claudeFile()));

        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
        expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledWith('va_sub', {
          access_token: CLAUDE_ACCESS,
          refresh_token: CLAUDE_REFRESH,
          expires_at: EXP_RFC3339,
        });
        const sent = JSON.stringify(fakeApi.connectVendorAccountImport.mock.calls);
        for (const raw of ['claudeAiOauth', 'subscriptionType', 'scopes', 'user:inference']) {
          expect(sent).not.toContain(raw);
        }
        expect(await screen.findByText(t.vendorConnectSuccess)).toBeInTheDocument();
      });

      it('sends only the access token when the file holds nothing else', async () => {
        const { fakeApi } = renderSubscription();
        await openDetail();

        upload(fileOf('.credentials.json', { claudeAiOauth: { accessToken: CLAUDE_ACCESS } }));

        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
        expect(fakeApi.connectVendorAccountImport.mock.calls[0][1]).toEqual({
          access_token: CLAUDE_ACCESS,
        });
      });

      it('fills the visible fields with what was extracted while the import is in flight', async () => {
        let resolveImport!: (account: VendorAccount) => void;
        const pending = new Promise<VendorAccount>((resolve) => {
          resolveImport = resolve;
        });
        const { fakeApi } = renderSubscription({}, { connectVendorAccountImport: () => pending });
        await openDetail();

        upload(fileOf('.credentials.json', claudeFile()));

        // The extracted tokens and expiry are shown (masked / as a local time) ...
        await waitFor(() =>
          expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue(CLAUDE_ACCESS),
        );
        expect(screen.getByLabelText(t.vendorConnectRefreshTokenLabel)).toHaveValue(CLAUDE_REFRESH);
        const expiry = new Date(EXP_RFC3339);
        const pad = (n: number) => String(n).padStart(2, '0');
        expect(screen.getByLabelText(t.vendorConnectExpiresAtLabel)).toHaveValue(
          `${expiry.getFullYear()}-${pad(expiry.getMonth() + 1)}-${pad(expiry.getDate())}` +
            `T${pad(expiry.getHours())}:${pad(expiry.getMinutes())}`,
        );
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveAttribute(
          'type',
          'password',
        );
        // ... the picker is busy-disabled meanwhile, and the SAME import was submitted.
        expect(screen.getByRole('button', { name: t.vendorConnectFileAction })).toBeDisabled();
        expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1);

        await act(async () =>
          resolveImport(
            makeVendorAccount({ ...SUBSCRIPTION, status: 'active', subscription_connected: true }),
          ),
        );
        await screen.findByText(t.vendorConnectSuccess);
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue('');
      });

      it('rejects a Codex file that has only an id_token, inline, without calling the API', async () => {
        const { fakeApi } = renderSubscription({ vendor: 'openai' });
        await openDetail();

        upload(
          fileOf('auth.json', {
            OPENAI_API_KEY: null,
            tokens: { id_token: ID_TOKEN, refresh_token: CODEX_REFRESH },
          }),
        );

        expect(
          await screen.findByText(t.vendorConnectFileErrorCodexIdTokenOnly),
        ).toBeInTheDocument();
        expect(fakeApi.connectVendorAccountImport).not.toHaveBeenCalled();
        // The id_token is not dropped into the form either, and no token is rendered.
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue('');
        expect(screen.getByLabelText(t.vendorConnectRefreshTokenLabel)).toHaveValue('');
        expect(document.body.innerHTML).not.toContain(ID_TOKEN);
        expect(screen.getByRole('button', { name: t.vendorConnectImportAction })).toBeDisabled();
      });

      // Every parser error code has its own localized text. The ambiguous case needs
      // a file name that does not say which of its two shapes to use.
      const PARSE_ERRORS: (readonly [string, string, unknown, MessageKey])[] = [
        ['not_json', 'credentials.json', '{ not json', 'vendorConnectFileErrorNotJson'],
        ['not_object', 'credentials.json', '[1, 2]', 'vendorConnectFileErrorNotObject'],
        [
          'unrecognised',
          'credentials.json',
          { hello: 'world' },
          'vendorConnectFileErrorUnrecognised',
        ],
        [
          'ambiguous',
          'credentials-copy.json',
          { ...claudeFile(), ...codexFile() },
          'vendorConnectFileErrorAmbiguous',
        ],
        [
          'claude_no_access_token',
          'credentials.json',
          { claudeAiOauth: { refreshToken: CLAUDE_REFRESH } },
          'vendorConnectFileErrorClaudeNoAccessToken',
        ],
        [
          'codex_id_token_only',
          'credentials.json',
          { tokens: { id_token: ID_TOKEN } },
          'vendorConnectFileErrorCodexIdTokenOnly',
        ],
        [
          'codex_no_access_token',
          'credentials.json',
          { OPENAI_API_KEY: 'sk-never-sent', tokens: null },
          'vendorConnectFileErrorCodexNoAccessToken',
        ],
      ];

      it.each(PARSE_ERRORS)(
        'shows the localized message for the %s parse error',
        async (_code, name, content, key) => {
          const { fakeApi } = renderSubscription();
          await openDetail();

          upload(fileOf(name, content));

          expect(await screen.findByText(t[key])).toBeInTheDocument();
          expect(fakeApi.connectVendorAccountImport).not.toHaveBeenCalled();
          expect(document.body.innerHTML).not.toContain('sk-never-sent');
        },
      );

      it('rejects a file for the other vendor, naming both, without calling the API', async () => {
        // A Codex file on an Anthropic account ...
        const first = renderSubscription();
        await openDetail();
        upload(fileOf('auth.json', codexFile()));
        expect(
          await screen.findByText(
            t.vendorConnectFileVendorMismatch(t.vendorOpenAI, t.vendorAnthropic),
          ),
        ).toBeInTheDocument();
        expect(first.fakeApi.connectVendorAccountImport).not.toHaveBeenCalled();
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue('');
        cleanup();

        // ... and a Claude Code file on an OpenAI account.
        const second = renderSubscription({ vendor: 'openai' });
        await openDetail();
        upload(fileOf('.credentials.json', claudeFile()));
        expect(
          await screen.findByText(
            t.vendorConnectFileVendorMismatch(t.vendorAnthropic, t.vendorOpenAI),
          ),
        ).toBeInTheDocument();
        expect(second.fakeApi.connectVendorAccountImport).not.toHaveBeenCalled();
      });

      it('rejects an oversized file before reading it', async () => {
        const read = vi.spyOn(FileReader.prototype, 'readAsText');
        const { fakeApi } = renderSubscription();
        await openDetail();

        upload(new File([new Uint8Array(1024 * 1024 + 1)], 'auth.json'));

        expect(await screen.findByText(t.vendorConnectFileTooLarge)).toBeInTheDocument();
        expect(read).not.toHaveBeenCalled();
        expect(fakeApi.connectVendorAccountImport).not.toHaveBeenCalled();
      });

      it('accepts a file at the size cap', async () => {
        const { fakeApi } = renderSubscription();
        await openDetail();
        // Valid JSON padded to exactly 1 MiB: the cap is inclusive.
        const json = JSON.stringify(claudeFile({ padding: '' }));
        const padded = json.slice(0, -1) + ' '.repeat(1024 * 1024 - json.length) + '}';
        expect(padded.length).toBe(1024 * 1024);

        upload(fileOf('.credentials.json', padded));

        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
      });

      it('says so when the browser cannot read the file', async () => {
        vi.spyOn(FileReader.prototype, 'readAsText').mockImplementation(function (
          this: FileReader,
        ) {
          queueMicrotask(() => this.dispatchEvent(new ProgressEvent('error')));
        });
        const { fakeApi } = renderSubscription();
        await openDetail();

        upload(fileOf('.credentials.json', claudeFile()));

        expect(await screen.findByText(t.vendorConnectFileReadFailed)).toBeInTheDocument();
        expect(fakeApi.connectVendorAccountImport).not.toHaveBeenCalled();
        expect(screen.getByRole('button', { name: t.vendorConnectFileAction })).toBeEnabled();
      });

      it('clears an earlier file error when the next file is chosen', async () => {
        const { fakeApi } = renderSubscription();
        await openDetail();

        upload(fileOf('credentials.json', '{ not json'));
        await screen.findByText(t.vendorConnectFileErrorNotJson);

        upload(fileOf('.credentials.json', claudeFile()));
        await waitFor(() => expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1));
        expect(screen.queryByText(t.vendorConnectFileErrorNotJson)).not.toBeInTheDocument();
      });

      it('shows the vendor rejecting the credentials as a localized toast and keeps the fields', async () => {
        const { fakeApi } = renderSubscription(
          {},
          { connectVendorAccountImport: refusal('vendor_account.connect_invalid_credentials') },
        );
        await openDetail();

        upload(fileOf('.credentials.json', claudeFile()));

        expect(
          await screen.findByText(
            `vendor_account.connect_invalid_credentials: ${t.errorVendorAccountConnectInvalidCredentials}`,
          ),
        ).toBeInTheDocument();
        expect(fakeApi.connectVendorAccountImport).toHaveBeenCalledTimes(1);
        // What was extracted stays visible, and the account stays not connected.
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toHaveValue(CLAUDE_ACCESS);
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();
        expect(screen.getByRole('button', { name: t.vendorConnectFileAction })).toBeEnabled();
      });

      it('shows the same localized message when a pasted token is rejected', async () => {
        renderSubscription(
          {},
          { connectVendorAccountImport: refusal('vendor_account.connect_invalid_credentials') },
        );
        await openDetail();

        fireEvent.change(screen.getByLabelText(t.vendorConnectAccessTokenLabel), {
          target: { value: 'dead-token' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));

        expect(
          await screen.findByText(
            `vendor_account.connect_invalid_credentials: ${t.errorVendorAccountConnectInvalidCredentials}`,
          ),
        ).toBeInTheDocument();
      });
    });

    describe('browser sign-in', () => {
      it('hides the open and paste steps until the connect has begun', async () => {
        renderSubscription();
        await openDetail();

        expect(screen.queryByRole('button', { name: t.vendorConnectOpenLogin })).toBeNull();
        expect(screen.queryByLabelText(t.vendorConnectCodeLabel)).toBeNull();
        expect(screen.queryByRole('button', { name: t.vendorConnectCompleteAction })).toBeNull();
      });

      it('begins, opens the sign-in URL in a new tab on its own click, then completes with the pasted code', async () => {
        const open = vi.spyOn(window, 'open').mockReturnValue(null);
        const { fakeApi } = renderSubscription();
        await openDetail();

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAction }));
        await waitFor(() =>
          expect(fakeApi.beginVendorAccountConnect).toHaveBeenCalledWith('va_sub'),
        );

        // Two clear steps: open the sign-in, then paste the code. Begin itself
        // opens nothing (a popup after an async call would be blocked).
        expect(open).not.toHaveBeenCalled();
        expect(await screen.findByText(t.vendorConnectStepOpen)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectStepPaste)).toBeInTheDocument();
        // The begin button is now a restart.
        expect(screen.queryByRole('button', { name: t.vendorConnectBeginAction })).toBeNull();
        expect(
          screen.getByRole('button', { name: t.vendorConnectBeginAgainAction }),
        ).toBeInTheDocument();

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectOpenLogin }));
        expect(open).toHaveBeenCalledTimes(1);
        expect(open).toHaveBeenCalledWith(AUTHORIZE_URL, '_blank', 'noopener');

        // Nothing to complete until something is pasted.
        const complete = screen.getByRole('button', { name: t.vendorConnectCompleteAction });
        expect(complete).toBeDisabled();
        fireEvent.change(screen.getByLabelText(t.vendorConnectCodeLabel), {
          target: { value: '  abc123#state-1  ' },
        });
        expect(complete).toBeEnabled();
        fireEvent.click(complete);

        await waitFor(() => expect(fakeApi.completeVendorAccountConnect).toHaveBeenCalledTimes(1));
        expect(fakeApi.completeVendorAccountConnect).toHaveBeenCalledWith(
          'va_sub',
          'abc123#state-1',
        );

        // Connected; the flow's steps and the pasted code are gone.
        expect(await screen.findByText(t.vendorConnectStatusConnected)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectSuccess)).toBeInTheDocument();
        expect(screen.queryByLabelText(t.vendorConnectCodeLabel)).toBeNull();
        expect(screen.queryByRole('button', { name: t.vendorConnectOpenLogin })).toBeNull();
        expect(
          screen.getByRole('button', { name: t.vendorConnectBeginAction }),
        ).toBeInTheDocument();
      });

      it('passes a pasted callback URL through untouched for the backend to parse', async () => {
        const { fakeApi } = renderSubscription({}, {});
        await openDetail();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAction }));
        const field = await screen.findByLabelText(t.vendorConnectCodeLabel);

        const callback = 'http://localhost:1455/auth/callback?code=xyz&state=s1';
        fireEvent.change(field, { target: { value: callback } });
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectCompleteAction }));

        await waitFor(() => expect(fakeApi.completeVendorAccountConnect).toHaveBeenCalledTimes(1));
        expect(fakeApi.completeVendorAccountConnect).toHaveBeenCalledWith('va_sub', callback);
      });

      it('keeps the form and the pasted code after a rejected code so the user can retry', async () => {
        let attempts = 0;
        const completeVendorAccountConnect = vi.fn<PortalApi['completeVendorAccountConnect']>(
          async (id) => {
            attempts += 1;
            if (attempts === 1) {
              throw new PortalApiError(400, 'vendor_account.connect_rejected', 'raw server text');
            }
            return makeVendorAccount({
              ...SUBSCRIPTION,
              id,
              api_key_set: false,
              subscription_connected: true,
            });
          },
        );
        renderSubscription({}, { completeVendorAccountConnect });
        await openDetail();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAction }));
        const field = await screen.findByLabelText(t.vendorConnectCodeLabel);
        fireEvent.change(field, { target: { value: 'typo-code' } });
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectCompleteAction }));

        expect(
          await screen.findByText(
            `vendor_account.connect_rejected: ${t.errorVendorAccountConnectRejected}`,
          ),
        ).toBeInTheDocument();
        // Still on step 2, still not connected, the paste is still there.
        expect(screen.getByLabelText(t.vendorConnectCodeLabel)).toHaveValue('typo-code');
        expect(screen.getByRole('button', { name: t.vendorConnectOpenLogin })).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();

        fireEvent.change(screen.getByLabelText(t.vendorConnectCodeLabel), {
          target: { value: 'right-code' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectCompleteAction }));

        await waitFor(() => expect(completeVendorAccountConnect).toHaveBeenCalledTimes(2));
        expect(completeVendorAccountConnect).toHaveBeenLastCalledWith('va_sub', 'right-code');
        expect(await screen.findByText(t.vendorConnectStatusConnected)).toBeInTheDocument();
      });

      it('starts over: a second begin clears the stale pasted code', async () => {
        const { fakeApi } = renderSubscription();
        await openDetail();
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAction }));
        fireEvent.change(await screen.findByLabelText(t.vendorConnectCodeLabel), {
          target: { value: 'stale' },
        });

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAgainAction }));

        await waitFor(() => expect(fakeApi.beginVendorAccountConnect).toHaveBeenCalledTimes(2));
        await waitFor(() =>
          expect(screen.getByLabelText(t.vendorConnectCodeLabel)).toHaveValue(''),
        );
      });

      it('shows a refused begin as a localized toast and offers no sign-in', async () => {
        renderSubscription(
          {},
          { beginVendorAccountConnect: refusal('vendor_account.connect_key_required') },
        );
        await openDetail();

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAction }));

        expect(
          await screen.findByText(
            `vendor_account.connect_key_required: ${t.errorVendorAccountConnectKeyRequired}`,
          ),
        ).toBeInTheDocument();
        expect(screen.queryByRole('button', { name: t.vendorConnectOpenLogin })).toBeNull();
      });

      it('never offers to open a URL that is not a web address', async () => {
        const open = vi.spyOn(window, 'open').mockReturnValue(null);
        renderSubscription(
          {},
          {
            beginVendorAccountConnect: async () => ({ authorize_url: 'javascript:alert(1)' }),
          },
        );
        await openDetail();

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAction }));

        expect(await screen.findByText(t.errorRequestFailed)).toBeInTheDocument();
        expect(screen.queryByRole('button', { name: t.vendorConnectOpenLogin })).toBeNull();
        expect(open).not.toHaveBeenCalled();
      });
    });

    describe('token import guide', () => {
      it.each([
        ['anthropic', 'Team Claude Max'],
        ['openai', 'ChatGPT Plus'],
      ] as const)(
        'is collapsed by default and explains where to find the token for an %s account',
        async (vendor, name) => {
          renderSubscription({ vendor, name });
          await openDetail();

          const summary = screen.getByRole('button', { name: t.vendorConnectGuideTitle });
          expect(summary).toHaveAttribute('aria-expanded', 'false');
          expect(screen.queryByText(t.vendorConnectGuideClaudeBody)).not.toBeInTheDocument();

          fireEvent.click(summary);

          expect(summary).toHaveAttribute('aria-expanded', 'true');
          expect(screen.getByText(t.vendorConnectGuideClaudeTitle)).toBeInTheDocument();
          expect(screen.getByText(t.vendorConnectGuideClaudeBody)).toBeInTheDocument();
          expect(screen.getByText(t.vendorConnectGuideCodexTitle)).toBeInTheDocument();
          expect(screen.getByText(t.vendorConnectGuideCodexBody)).toBeInTheDocument();
        },
      );

      it('sits in the token-import section, above the token fields', async () => {
        renderSubscription();
        await openDetail();

        const importSection = screen
          .getByRole('heading', { name: t.vendorConnectImportTitle })
          .closest('section')!;
        const summary = within(importSection).getByRole('button', {
          name: t.vendorConnectGuideTitle,
        });
        const accessToken = within(importSection).getByLabelText(t.vendorConnectAccessTokenLabel);
        expect(
          summary.compareDocumentPosition(accessToken) & Node.DOCUMENT_POSITION_FOLLOWING,
        ).toBeTruthy();
      });

      it('names the files, fields and command of both command-line clients', () => {
        // The facts the guide exists for, pinned in both languages.
        expect(t.vendorConnectGuideClaudeBody).toContain('~/.claude/.credentials.json');
        expect(t.vendorConnectGuideClaudeBody).toContain('claudeAiOauth.accessToken');
        expect(t.vendorConnectGuideClaudeBody).toContain('Claude Code-credentials');
        expect(t.vendorConnectGuideClaudeBody).toContain('claude setup-token');
        expect(t.vendorConnectGuideCodexBody).toContain('~/.codex/auth.json');
        expect(t.vendorConnectGuideCodexBody).toContain('tokens.access_token');
      });
    });

    describe('device code', () => {
      const OPENAI: Partial<VendorAccount> = {
        vendor: 'openai',
        name: 'ChatGPT Plus',
      };
      const upstreamFailure = () =>
        new PortalApiError(502, 'vendor_account.connect_upstream_failed', 'raw upstream text');
      const rejection = () =>
        new PortalApiError(400, 'vendor_account.connect_rejected', 'raw server text');

      /** A poll mock answering `steps` in order; the last step repeats. A step is
       * a {connected} answer or an error to throw. */
      function pollSteps(...steps: ({ connected: boolean } | Error)[]) {
        let i = 0;
        return async () => {
          const step = steps[Math.min(i, steps.length - 1)];
          i += 1;
          if (step instanceof Error) throw step;
          return step;
        };
      }

      /** Open the detail view with real timers, THEN fake them: the poll's
       * setTimeout is then the faked one, and the list/detail loading is not. */
      async function openOpenAIDetail(opts: Parameters<typeof renderView>[0] = {}) {
        const view = renderSubscription(OPENAI, opts);
        await openDetail();
        vi.useFakeTimers();
        return view;
      }

      async function advance(ms: number) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(ms);
        });
      }

      async function startDevice() {
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }));
        await advance(0);
      }

      it('is offered for an OpenAI subscription, as a third method', async () => {
        renderSubscription(OPENAI);
        await openDetail();

        expect(screen.getByRole('heading', { name: t.vendorConnectDeviceTitle })).toBeVisible();
        expect(
          screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).toBeEnabled();
        // The other two methods are still there.
        expect(screen.getByRole('button', { name: t.vendorConnectBeginAction })).toBeEnabled();
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toBeInTheDocument();
      });

      it('is hidden for an Anthropic subscription (Anthropic has no device login)', async () => {
        renderSubscription({ vendor: 'anthropic' });
        await openDetail();

        expect(screen.queryByText(t.vendorConnectDeviceTitle)).not.toBeInTheDocument();
        expect(
          screen.queryByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).not.toBeInTheDocument();
        // Still offered: the other two methods.
        expect(
          screen.getByRole('button', { name: t.vendorConnectBeginAction }),
        ).toBeInTheDocument();
        expect(screen.getByLabelText(t.vendorConnectAccessTokenLabel)).toBeInTheDocument();
      });

      it('is absent on an api_key account, which has no connect panel at all', async () => {
        renderView({ accounts: [makeVendorAccount({ vendor: 'openai', auth_type: 'api_key' })] });
        fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
        await screen.findByText(t.vendorAccountSettingsTitle);

        expect(screen.queryByText(t.vendorConnectDeviceTitle)).not.toBeInTheDocument();
      });

      it('begins without a body, shows the user code and the page, and opens it in a new tab only on its own click', async () => {
        const open = vi.spyOn(window, 'open').mockReturnValue(null);
        const { fakeApi } = await openOpenAIDetail();

        await startDevice();

        expect(fakeApi.beginVendorAccountDeviceConnect).toHaveBeenCalledWith('va_sub');
        // The pairing code is shown prominently, with its label and the steps.
        expect(screen.getByText(DEVICE_CODE)).toBeVisible();
        expect(screen.getByText(t.vendorConnectDeviceCodeLabel)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectDeviceInstructions)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectDeviceWaiting)).toBeInTheDocument();
        // The page is also written out, so it can be opened on another device.
        expect(screen.getByText(DEVICE_URL)).toBeInTheDocument();
        // Begin itself opens nothing (a popup after an async call would be blocked).
        expect(open).not.toHaveBeenCalled();

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectDeviceOpenAction }));
        expect(open).toHaveBeenCalledTimes(1);
        expect(open).toHaveBeenCalledWith(DEVICE_URL, '_blank', 'noopener');
        // Not polled yet: the first poll comes one interval after begin.
        expect(fakeApi.pollVendorAccountDeviceConnect).not.toHaveBeenCalled();
      });

      it('polls on the interval while the approval is pending, then stops on connected and refreshes the account', async () => {
        const { fakeApi } = await openOpenAIDetail({
          pollVendorAccountDeviceConnect: pollSteps(
            { connected: false },
            { connected: false },
            { connected: true },
          ),
        });
        await startDevice();
        const polls = () => fakeApi.pollVendorAccountDeviceConnect.mock.calls.length;

        await advance(DEVICE_POLL_INTERVAL_MS - 1);
        expect(polls()).toBe(0);
        await advance(1);
        expect(polls()).toBe(1);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenLastCalledWith('va_sub');
        // Still pending: the code stays on screen, nothing connected yet.
        expect(screen.getByText(DEVICE_CODE)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();

        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(2);
        expect(fakeApi.vendorAccount).not.toHaveBeenCalled();

        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(3);

        // Connected: the account was re-read, the status flipped, the code is gone.
        expect(fakeApi.vendorAccount).toHaveBeenCalledWith('va_sub');
        expect(screen.getByText(t.vendorConnectStatusConnected)).toHaveAttribute(
          'data-status',
          'active',
        );
        expect(screen.getByText(t.vendorConnectSuccess)).toBeInTheDocument();
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        expect(
          screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).toBeEnabled();

        // ... and the loop is over for good.
        await advance(DEVICE_POLL_INTERVAL_MS * 10);
        expect(polls()).toBe(3);
      });

      it('lists the account as stored after the device connect', async () => {
        await openOpenAIDetail({ pollVendorAccountDeviceConnect: pollSteps({ connected: true }) });
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(screen.getByText(t.vendorConnectStatusConnected)).toBeInTheDocument();

        vi.useRealTimers();
        fireEvent.click(screen.getByRole('button', { name: t.providers }));
        const row = (await screen.findByText('ChatGPT Plus')).closest('tr')!;
        expect(within(row).getByText(t.vendorAccountCredentialSet)).toBeInTheDocument();
      });

      it('still shows the account as connected when re-reading it fails', async () => {
        const { fakeApi } = await openOpenAIDetail({
          pollVendorAccountDeviceConnect: pollSteps({ connected: true }),
          vendorAccount: async () => {
            throw new PortalApiError(500, 'vendor_account.get_failed', 'raw server text');
          },
        });
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);

        // Two reads, both failing: the connect's own re-read, then the usage
        // panel's read once the account reads as connected (a failed usage read
        // stays silent, see the usage & limits tests).
        expect(fakeApi.vendorAccount).toHaveBeenCalledTimes(2);
        expect(fakeApi.vendorAccount).toHaveBeenNthCalledWith(1, 'va_sub');
        expect(fakeApi.vendorAccount).toHaveBeenNthCalledWith(2, 'va_sub');
        expect(screen.getByText(t.vendorConnectStatusConnected)).toHaveAttribute(
          'data-status',
          'active',
        );
        expect(screen.getByText(t.vendorConnectSuccess)).toBeInTheDocument();
      });

      it.each([
        ['a 502 vendor_account.connect_upstream_failed', upstreamFailure],
        [
          'a proxy-level 502 without an error body',
          () => new PortalApiError(502, 'request.failed', 'Bad Gateway'),
        ],
        ['a 503 from the proxy', () => new PortalApiError(503, 'request.failed', 'Unavailable')],
        ['a network failure of the browser itself', () => new TypeError('Failed to fetch')],
      ])('keeps polling through %s and connects once the vendor answers again', async (_, blip) => {
        const { fakeApi } = await openOpenAIDetail({
          pollVendorAccountDeviceConnect: pollSteps(
            blip(),
            blip(),
            { connected: false },
            { connected: true },
          ),
        });
        await startDevice();
        const polls = () => fakeApi.pollVendorAccountDeviceConnect.mock.calls.length;

        // First blip: no error toast, no stop; the panel says it is still trying.
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(1);
        expect(screen.getByText(t.vendorConnectDeviceRetrying)).toBeInTheDocument();
        expect(screen.getByText(DEVICE_CODE)).toBeInTheDocument();
        expect(screen.queryByText(/vendor_account\./)).not.toBeInTheDocument();

        // Second blip: still polling.
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(2);
        expect(screen.getByText(t.vendorConnectDeviceRetrying)).toBeInTheDocument();

        // A good poll clears the retry hint; the loop carries on.
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(3);
        expect(screen.queryByText(t.vendorConnectDeviceRetrying)).not.toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectDeviceWaiting)).toBeInTheDocument();

        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(4);
        expect(screen.getByText(t.vendorConnectStatusConnected)).toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectSuccess)).toBeInTheDocument();
      });

      it.each([
        [
          'a vendor refusal (400 connect_rejected)',
          rejection,
          `vendor_account.connect_rejected: ${t.errorVendorAccountConnectRejected}`,
        ],
        [
          'an expired login (400 device_connect_state)',
          () => new PortalApiError(400, 'vendor_account.device_connect_state', 'raw server text'),
          `vendor_account.device_connect_state: ${t.errorVendorAccountDeviceConnectState}`,
        ],
        [
          'a disabled module (409)',
          () => new PortalApiError(409, 'vendor_accounts.module_disabled', 'raw server text'),
          `vendor_accounts.module_disabled: ${t.errorVendorAccountsModuleDisabled}`,
        ],
      ])('stops on %s, says why, and offers a clean restart', async (_, failure, message) => {
        const { fakeApi } = await openOpenAIDetail({
          pollVendorAccountDeviceConnect: pollSteps({ connected: false }, failure()),
        });
        await startDevice();
        const polls = () => fakeApi.pollVendorAccountDeviceConnect.mock.calls.length;

        await advance(DEVICE_POLL_INTERVAL_MS * 2);
        expect(polls()).toBe(2);

        expect(screen.getByText(message)).toBeInTheDocument();
        // The dead login's code is gone and the start button is back.
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        expect(
          screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).toBeEnabled();
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();
        expect(fakeApi.vendorAccount).not.toHaveBeenCalled();

        // No further polls, however long we wait.
        await advance(DEVICE_POLL_INTERVAL_MS * 20);
        expect(polls()).toBe(2);
      });

      it('restarts after a rejection with a fresh begin, a new code and a new poll loop', async () => {
        let begins = 0;
        const { fakeApi } = await openOpenAIDetail({
          beginVendorAccountDeviceConnect: async () => {
            begins += 1;
            return {
              user_code: begins === 1 ? 'FIRST-1111' : 'SECOND-2222',
              verification_url: DEVICE_URL,
            };
          },
          pollVendorAccountDeviceConnect: pollSteps(rejection(), { connected: true }),
        });
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(screen.queryByText('FIRST-1111')).not.toBeInTheDocument();

        await startDevice();
        expect(screen.getByText('SECOND-2222')).toBeInTheDocument();
        expect(fakeApi.beginVendorAccountDeviceConnect).toHaveBeenCalledTimes(2);

        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(screen.getByText(t.vendorConnectStatusConnected)).toBeInTheDocument();
      });

      it('gives up after the timeout, says so, and lets the user start again', async () => {
        const { fakeApi } = await openOpenAIDetail();
        await startDevice();
        const polls = () => fakeApi.pollVendorAccountDeviceConnect.mock.calls.length;

        await advance(DEVICE_POLL_TIMEOUT_MS - DEVICE_POLL_INTERVAL_MS);
        expect(screen.getByText(DEVICE_CODE)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorConnectDeviceTimedOut)).not.toBeInTheDocument();

        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(DEVICE_POLL_TIMEOUT_MS / DEVICE_POLL_INTERVAL_MS);
        expect(screen.getByText(t.vendorConnectDeviceTimedOut)).toBeInTheDocument();
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        expect(
          screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).toBeEnabled();

        // The loop is over.
        await advance(DEVICE_POLL_INTERVAL_MS * 10);
        expect(polls()).toBe(DEVICE_POLL_TIMEOUT_MS / DEVICE_POLL_INTERVAL_MS);

        // Starting again clears the notice and polls anew.
        await startDevice();
        expect(screen.queryByText(t.vendorConnectDeviceTimedOut)).not.toBeInTheDocument();
        expect(screen.getByText(DEVICE_CODE)).toBeInTheDocument();
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(DEVICE_POLL_TIMEOUT_MS / DEVICE_POLL_INTERVAL_MS + 1);
      });

      it('honours an approval that lands on the very last poll before the timeout', async () => {
        const last = DEVICE_POLL_TIMEOUT_MS / DEVICE_POLL_INTERVAL_MS;
        let calls = 0;
        await openOpenAIDetail({
          pollVendorAccountDeviceConnect: async () => {
            calls += 1;
            return { connected: calls === last };
          },
        });
        await startDevice();

        await advance(DEVICE_POLL_TIMEOUT_MS);

        expect(calls).toBe(last);
        expect(screen.queryByText(t.vendorConnectDeviceTimedOut)).not.toBeInTheDocument();
        expect(screen.getByText(t.vendorConnectStatusConnected)).toBeInTheDocument();
      });

      it('never overlaps polls: the next one is scheduled only after the previous answered', async () => {
        let resolvePoll: (value: { connected: boolean }) => void = () => undefined;
        const { fakeApi } = await openOpenAIDetail({
          pollVendorAccountDeviceConnect: () =>
            new Promise((resolve) => {
              resolvePoll = resolve;
            }),
        });
        await startDevice();
        const polls = () => fakeApi.pollVendorAccountDeviceConnect.mock.calls.length;

        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(1);
        // The poll is slow: several intervals pass without a second call.
        await advance(DEVICE_POLL_INTERVAL_MS * 4);
        expect(polls()).toBe(1);

        await act(async () => {
          resolvePoll({ connected: false });
        });
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(polls()).toBe(2);
      });

      it('stops polling when the user cancels, and a late answer changes nothing', async () => {
        let resolvePoll: (value: { connected: boolean }) => void = () => undefined;
        const { fakeApi } = await openOpenAIDetail({
          pollVendorAccountDeviceConnect: () =>
            new Promise((resolve) => {
              resolvePoll = resolve;
            }),
        });
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);

        fireEvent.click(screen.getByRole('button', { name: t.cancel }));
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        expect(
          screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).toBeEnabled();

        // The in-flight poll answers "connected" after the cancel: it is dropped.
        await act(async () => {
          resolvePoll({ connected: true });
        });
        await advance(DEVICE_POLL_INTERVAL_MS * 10);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);
        expect(fakeApi.vendorAccount).not.toHaveBeenCalled();
        expect(screen.getByText(t.vendorConnectStatusNotConnected)).toBeInTheDocument();
      });

      it('starts over while waiting: a new begin replaces the code and the old loop is dropped', async () => {
        let begins = 0;
        const { fakeApi } = await openOpenAIDetail({
          beginVendorAccountDeviceConnect: async () => {
            begins += 1;
            return { user_code: `CODE-${begins}`, verification_url: DEVICE_URL };
          },
        });
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS * 2);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(2);

        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectBeginAgainAction }));
        await advance(0);
        expect(screen.getByText('CODE-2')).toBeInTheDocument();
        expect(screen.queryByText('CODE-1')).not.toBeInTheDocument();

        // One loop only: exactly one poll per interval after the restart.
        await advance(DEVICE_POLL_INTERVAL_MS * 3);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(5);
      });

      it('leaves no timer behind when the user leaves the detail view', async () => {
        const { fakeApi } = await openOpenAIDetail();
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);

        fireEvent.click(screen.getByRole('button', { name: t.providers }));
        await advance(0);
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        expect(vi.getTimerCount()).toBe(0);

        await advance(DEVICE_POLL_INTERVAL_MS * 10);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);
      });

      it('leaves no timer behind when the whole view unmounts', async () => {
        const { fakeApi, unmount } = await openOpenAIDetail();
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);

        unmount();
        expect(vi.getTimerCount()).toBe(0);
        await advance(DEVICE_POLL_INTERVAL_MS * 10);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);
      });

      it('stops waiting when another method connects the account in the meantime', async () => {
        const { fakeApi } = await openOpenAIDetail();
        await startDevice();
        await advance(DEVICE_POLL_INTERVAL_MS);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);

        fireEvent.change(screen.getByLabelText(t.vendorConnectAccessTokenLabel), {
          target: { value: 'at-secret-access' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.vendorConnectImportAction }));
        await advance(0);

        expect(screen.getByText(t.vendorConnectStatusConnected)).toBeInTheDocument();
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        await advance(DEVICE_POLL_INTERVAL_MS * 10);
        expect(fakeApi.pollVendorAccountDeviceConnect).toHaveBeenCalledTimes(1);
      });

      it('shows a refused begin as a localized toast and starts no poll', async () => {
        const { fakeApi } = await openOpenAIDetail({
          beginVendorAccountDeviceConnect: async () => {
            throw new PortalApiError(400, 'vendor_account.device_not_supported', 'raw server text');
          },
        });
        await startDevice();

        expect(
          screen.getByText(
            `vendor_account.device_not_supported: ${t.errorVendorAccountDeviceNotSupported}`,
          ),
        ).toBeInTheDocument();
        expect(screen.queryByText(t.vendorConnectDeviceWaiting)).not.toBeInTheDocument();
        expect(
          screen.getByRole('button', { name: t.vendorConnectDeviceStartAction }),
        ).toBeEnabled();
        await advance(DEVICE_POLL_INTERVAL_MS * 5);
        expect(fakeApi.pollVendorAccountDeviceConnect).not.toHaveBeenCalled();
      });

      it('never offers to open a verification URL that is not a web address', async () => {
        const open = vi.spyOn(window, 'open').mockReturnValue(null);
        const { fakeApi } = await openOpenAIDetail({
          beginVendorAccountDeviceConnect: async () => ({
            user_code: DEVICE_CODE,
            verification_url: 'javascript:alert(1)',
          }),
        });
        await startDevice();

        expect(screen.getByText(t.errorRequestFailed)).toBeInTheDocument();
        expect(screen.queryByText(DEVICE_CODE)).not.toBeInTheDocument();
        expect(
          screen.queryByRole('button', { name: t.vendorConnectDeviceOpenAction }),
        ).not.toBeInTheDocument();
        expect(open).not.toHaveBeenCalled();
        await advance(DEVICE_POLL_INTERVAL_MS * 5);
        expect(fakeApi.pollVendorAccountDeviceConnect).not.toHaveBeenCalled();
      });
    });
  });

  describe(`VendorAccountsView test connection [${locale}]`, () => {
    async function openDetail(name = 'Work OpenAI') {
      const row = (await screen.findByText(name)).closest('tr')!;
      fireEvent.click(within(row).getByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.vendorAccountSettingsTitle);
    }

    const testButton = () => screen.getByRole('button', { name: t.vendorCheckAction });
    const verdict = () => screen.getByRole('status');

    function deferred<T>() {
      let resolve!: (value: T) => void;
      let reject!: (reason: unknown) => void;
      const promise = new Promise<T>((res, rej) => {
        resolve = res;
        reject = rej;
      });
      return { promise, resolve, reject };
    }

    it('offers the test button with a note that only the credentials are checked, not a model', async () => {
      const { fakeApi } = renderView();
      await openDetail();

      expect(testButton()).toBeEnabled();
      expect(screen.getByText(t.vendorCheckIntro)).toBeInTheDocument();
      // Nothing is sent, and no verdict is shown, until the user asks.
      expect(fakeApi.testConnection).not.toHaveBeenCalled();
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
    });

    it('is offered on a subscription that is not connected too (the verdict then says why it cannot be checked)', async () => {
      renderView({ accounts: [makeVendorAccount(SUBSCRIPTION)] });
      await openDetail('Team Claude Max');

      expect(testButton()).toBeEnabled();
    });

    it('is not offered on the list or the create form', async () => {
      renderView();
      await screen.findByText('Work OpenAI');
      expect(screen.queryByRole('button', { name: t.vendorCheckAction })).not.toBeInTheDocument();

      fireEvent.click(screen.getByRole('button', { name: t.vendorAccountCreate }));
      await screen.findByLabelText(t.vendorAccountNameLabel);
      expect(screen.queryByRole('button', { name: t.vendorCheckAction })).not.toBeInTheDocument();
    });

    it('checks the open account by id and shows a valid verdict as success', async () => {
      const { fakeApi } = renderView();
      await openDetail();

      fireEvent.click(testButton());

      expect(await screen.findByText(t.vendorCheckValid)).toBeInTheDocument();
      expect(fakeApi.testConnection).toHaveBeenCalledTimes(1);
      expect(fakeApi.testConnection).toHaveBeenCalledWith('va_1');
      expect(verdict()).toHaveClass('MuiAlert-colorSuccess');
      expect(within(verdict()).getByText(t.vendorCheckValid)).toBeInTheDocument();
    });

    it('shows an invalid verdict as an error', async () => {
      renderView({
        testConnection: async () =>
          makeCheck({ status: 'invalid', detail: 'the vendor rejected the credential (401)' }),
      });
      await openDetail();

      fireEvent.click(testButton());

      expect(await screen.findByText(t.vendorCheckInvalid)).toBeInTheDocument();
      expect(verdict()).toHaveClass('MuiAlert-colorError');
    });

    it('shows an unverifiable verdict neutrally: neither success nor error', async () => {
      renderView({
        testConnection: async () =>
          makeCheck({ status: 'unverifiable', detail: 'the vendor could not be reached' }),
      });
      await openDetail();

      fireEvent.click(testButton());

      expect(await screen.findByText(t.vendorCheckUnverifiable)).toBeInTheDocument();
      expect(verdict()).toHaveClass('MuiAlert-colorInfo');
      expect(verdict()).not.toHaveClass('MuiAlert-colorSuccess');
      expect(verdict()).not.toHaveClass('MuiAlert-colorError');
    });

    it('words the three verdicts differently, so none reads as another', () => {
      const texts = [t.vendorCheckValid, t.vendorCheckInvalid, t.vendorCheckUnverifiable];
      expect(new Set(texts).size).toBe(3);
    });

    it('leads with the localized verdict and keeps the English detail as secondary technical text', async () => {
      const detail = 'the vendor rejected the credential (invalid_api_key)';
      renderView({ testConnection: async () => makeCheck({ status: 'invalid', detail }) });
      await openDetail();

      fireEvent.click(testButton());

      await screen.findByText(t.vendorCheckInvalid);
      // The detail is labelled as technical, never the verdict's headline.
      const secondary = within(verdict()).getByText(t.vendorCheckDetail(detail));
      expect(secondary).toBeInTheDocument();
      expect(screen.queryByText(detail, { exact: true })).not.toBeInTheDocument();
      // The localized headline comes first in the verdict.
      expect(verdict().textContent?.indexOf(t.vendorCheckInvalid)).toBeLessThan(
        verdict().textContent?.indexOf(detail) ?? -1,
      );
    });

    it('shows no technical line when the backend sent no detail', async () => {
      renderView({ testConnection: async () => makeCheck({ detail: '' }) });
      await openDetail();

      fireEvent.click(testButton());

      await screen.findByText(t.vendorCheckValid);
      expect(verdict().textContent).toBe(t.vendorCheckValid);
    });

    it('disables the button while the check runs, sends it once, and re-enables it with the verdict', async () => {
      const pending = deferred<VendorConnectionCheck>();
      const { fakeApi } = renderView({ testConnection: () => pending.promise });
      await openDetail();

      fireEvent.click(testButton());

      await waitFor(() => expect(testButton()).toBeDisabled());
      fireEvent.click(testButton());
      expect(fakeApi.testConnection).toHaveBeenCalledTimes(1);
      expect(screen.queryByRole('status')).not.toBeInTheDocument();

      await act(async () => pending.resolve(makeCheck()));

      expect(await screen.findByText(t.vendorCheckValid)).toBeInTheDocument();
      expect(testButton()).toBeEnabled();
    });

    it('hides the previous verdict while it checks again, then shows the new one', async () => {
      const second = deferred<VendorConnectionCheck>();
      const testConnection = vi
        .fn<PortalApi['testConnection']>()
        .mockResolvedValueOnce(makeCheck({ status: 'valid' }))
        .mockReturnValueOnce(second.promise);
      renderView({ testConnection });
      await openDetail();

      fireEvent.click(testButton());
      await screen.findByText(t.vendorCheckValid);

      fireEvent.click(testButton());
      await waitFor(() => expect(testButton()).toBeDisabled());
      // A stale "valid" must not stay on screen next to a check that is running.
      expect(screen.queryByText(t.vendorCheckValid)).not.toBeInTheDocument();

      await act(async () => second.resolve(makeCheck({ status: 'invalid' })));
      expect(await screen.findByText(t.vendorCheckInvalid)).toBeInTheDocument();
      expect(screen.queryByText(t.vendorCheckValid)).not.toBeInTheDocument();
    });

    it('shows a thrown error as the localized toast, not as a verdict, and re-enables the button', async () => {
      renderView({
        testConnection: async () => {
          throw new PortalApiError(500, 'vendor_account.check_failed', 'raw server text');
        },
      });
      await openDetail();

      fireEvent.click(testButton());

      expect(
        await screen.findByText(`vendor_account.check_failed: ${t.errorVendorAccountCheckFailed}`),
      ).toBeInTheDocument();
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorCheckValid)).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorCheckInvalid)).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorCheckUnverifiable)).not.toBeInTheDocument();
      expect(testButton()).toBeEnabled();
    });

    it('clears an earlier verdict when the next check throws', async () => {
      const testConnection = vi
        .fn<PortalApi['testConnection']>()
        .mockResolvedValueOnce(makeCheck())
        .mockRejectedValueOnce(new PortalApiError(500, 'vendor_account.check_failed', 'raw'));
      renderView({ testConnection });
      await openDetail();

      fireEvent.click(testButton());
      await screen.findByText(t.vendorCheckValid);
      fireEvent.click(testButton());

      await screen.findByText(`vendor_account.check_failed: ${t.errorVendorAccountCheckFailed}`);
      expect(screen.queryByText(t.vendorCheckValid)).not.toBeInTheDocument();
    });

    it("does not show one account's verdict on another account", async () => {
      renderView({
        accounts: [
          makeVendorAccount(),
          makeVendorAccount({ id: 'va_other', name: 'Other OpenAI' }),
        ],
      });
      await openDetail();
      fireEvent.click(testButton());
      await screen.findByText(t.vendorCheckValid);

      fireEvent.click(screen.getByRole('button', { name: t.providers }));
      await openDetail('Other OpenAI');

      expect(screen.queryByRole('status')).not.toBeInTheDocument();
      expect(testButton()).toBeEnabled();
    });

    it('drops a verdict once the account was saved, because the credential may have changed', async () => {
      renderView({
        updateVendorAccount: async (id) =>
          makeVendorAccount({ id, updated_at: '2026-10-08T11:00:00Z' }),
      });
      await openDetail();
      fireEvent.click(testButton());
      await screen.findByText(t.vendorCheckValid);

      fireEvent.change(screen.getByLabelText(t.vendorAccountApiKeyLabel), {
        target: { value: 'sk-rotated' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));

      await screen.findByText(t.save, { selector: '[role="alert"] *' });
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorCheckValid)).not.toBeInTheDocument();
    });

    it('ignores a result that arrives after the user left the account (no verdict, no toast)', async () => {
      const pending = deferred<VendorConnectionCheck>();
      const { fakeApi } = renderView({ testConnection: () => pending.promise });
      await openDetail();
      fireEvent.click(testButton());
      await waitFor(() => expect(fakeApi.testConnection).toHaveBeenCalledTimes(1));

      fireEvent.click(screen.getByRole('button', { name: t.providers }));
      await screen.findByRole('button', { name: t.vendorAccountCreate });
      await act(async () =>
        pending.reject(new PortalApiError(500, 'vendor_account.check_failed', 'raw')),
      );

      expect(screen.queryByText(/vendor_account\.check_failed/)).not.toBeInTheDocument();
      // Back on the same account: it has no verdict from the abandoned check.
      await openDetail();
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
      expect(testButton()).toBeEnabled();
    });
  });

  describe(`VendorAccountsView usage & limits [${locale}]`, () => {
    // The usage snapshot rides on the single-account GET only: the list rows
    // carry none, so the detail view reads it when it opens.
    const withUsage = (usage?: VendorAccountUsage) => async (id: string) =>
      makeVendorAccount({ ...SUBSCRIPTION, id, subscription_connected: true, usage });

    async function openDetail() {
      fireEvent.click(await screen.findByRole('button', { name: t.modelDetailsAction }));
      await screen.findByText(t.vendorAccountSettingsTitle);
    }

    it('shows both windows with their reset countdown and the credit balance for a connected subscription', async () => {
      const { fakeApi } = renderView({
        accounts: [makeVendorAccount({ ...SUBSCRIPTION, subscription_connected: true })],
        vendorAccount: withUsage(makeUsage({ credit_balance: '12.34' })),
      });
      await openDetail();

      expect(await screen.findByText(t.vendorUsageTitle)).toBeInTheDocument();
      expect(fakeApi.vendorAccount).toHaveBeenCalledWith('va_sub');
      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toHaveAttribute(
        'aria-valuenow',
        '42',
      );
      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toHaveAttribute(
        'aria-valuenow',
        '7',
      );
      expect(screen.getByText(t.vendorUsageResetsIn('2 h 14 min'))).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageResetsIn('3 d 4 h'))).toBeInTheDocument();
      expect(screen.getByText('12.34')).toBeInTheDocument();
    });

    it('shows "no data yet" for a window the gateway has not observed, with no bar for it', async () => {
      renderView({
        accounts: [makeVendorAccount({ ...SUBSCRIPTION, subscription_connected: true })],
        vendorAccount: withUsage(makeUsage({ weekly_pct: -1, weekly_reset_at: null })),
      });
      await openDetail();

      await screen.findByRole('progressbar', { name: t.vendorUsageFiveHour });
      expect(screen.getByText(t.vendorUsageNoData)).toBeInTheDocument();
      expect(
        screen.queryByRole('progressbar', { name: t.vendorUsageWeekly }),
      ).not.toBeInTheDocument();
    });

    it('hides the panel for an account the gateway has no snapshot for', async () => {
      const { fakeApi } = renderView({
        accounts: [makeVendorAccount({ ...SUBSCRIPTION, subscription_connected: true })],
        vendorAccount: withUsage(undefined),
      });
      await openDetail();

      await waitFor(() => expect(fakeApi.vendorAccount).toHaveBeenCalledWith('va_sub'));
      expect(screen.queryByText(t.vendorUsageTitle)).not.toBeInTheDocument();
    });

    it('hides the panel for a snapshot that knows no window and no balance yet', async () => {
      const { fakeApi } = renderView({
        accounts: [makeVendorAccount({ ...SUBSCRIPTION, subscription_connected: true })],
        vendorAccount: withUsage(
          makeUsage({
            five_hour_pct: -1,
            five_hour_reset_at: null,
            weekly_pct: -1,
            weekly_reset_at: null,
          }),
        ),
      });
      await openDetail();

      await waitFor(() => expect(fakeApi.vendorAccount).toHaveBeenCalledWith('va_sub'));
      expect(screen.queryByText(t.vendorUsageTitle)).not.toBeInTheDocument();
    });

    it('is also available for an api-key account that has a snapshot', async () => {
      renderView({
        vendorAccount: async (id: string) => makeVendorAccount({ id, usage: makeUsage() }),
      });
      await openDetail();

      expect(await screen.findByText(t.vendorUsageTitle)).toBeInTheDocument();
      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeInTheDocument();
    });

    it('does not read a snapshot for a subscription that was never connected', async () => {
      const { fakeApi } = renderView({
        accounts: [makeVendorAccount(SUBSCRIPTION)],
        vendorAccount: withUsage(makeUsage()),
      });
      await openDetail();

      expect(fakeApi.vendorAccount).not.toHaveBeenCalled();
      expect(screen.queryByText(t.vendorUsageTitle)).not.toBeInTheDocument();
    });

    it('leaves the panel out, without an error toast, when the snapshot cannot be read', async () => {
      renderView({
        accounts: [makeVendorAccount({ ...SUBSCRIPTION, subscription_connected: true })],
        vendorAccount: async () => {
          throw new PortalApiError(500, 'vendor_account.get_failed', 'raw server text');
        },
      });
      await openDetail();

      // The settings panel is fully usable; only the optional usage panel is absent.
      await waitFor(() => expect(screen.queryByText(t.vendorUsageTitle)).not.toBeInTheDocument());
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(screen.getByLabelText(t.vendorAccountNameLabel)).toBeInTheDocument();
    });

    it('keeps the panel after the settings are saved (the PATCH answer carries no snapshot)', async () => {
      const { fakeApi } = renderView({
        vendorAccount: async (id: string) => makeVendorAccount({ id, usage: makeUsage() }),
      });
      await openDetail();
      await screen.findByText(t.vendorUsageTitle);

      fireEvent.change(screen.getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Renamed' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));
      await waitFor(() => expect(fakeApi.updateVendorAccount).toHaveBeenCalled());

      expect(await screen.findByText(t.save, { selector: '[role="alert"] *' })).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageTitle)).toBeInTheDocument();
      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeInTheDocument();
    });

    it('never asks for a snapshot on the list (it would be one read per row)', async () => {
      const { fakeApi } = renderView({
        accounts: [
          makeVendorAccount({ id: 'va_a' }),
          makeVendorAccount({ id: 'va_b', name: 'Second' }),
        ],
      });
      await screen.findByText('Second');

      expect(fakeApi.vendorAccount).not.toHaveBeenCalled();
    });
  });
}
