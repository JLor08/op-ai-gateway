// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TokenList } from './TokenList';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import type {
  CreateTokenRequest,
  PortalToken,
  UpdateTokenRequest,
  VendorAccountGroup,
} from '../api';

afterEach(cleanup);

function makeGroup(overrides: Partial<VendorAccountGroup> = {}): VendorAccountGroup {
  return {
    id: 'vag_1',
    vendor: 'anthropic',
    name: 'Claude pool',
    model_prefix: 'pool/',
    status: 'active',
    thresholds: { five_hour_pct: 99, weekly_pct: 99, credits_pct: 99 },
    active_member_id: '',
    members: [],
    created_at: '2026-11-24T10:00:00Z',
    updated_at: '2026-11-24T10:00:00Z',
    ...overrides,
  };
}

function makeToken(overrides: Partial<PortalToken> = {}): PortalToken {
  return {
    id: 'tok_1',
    name: 'Dev Token',
    secret_prefix: 'dev-secr',
    status: 'active',
    scopes: ['gateway:use'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-07-10T12:00:00Z',
    model_override: '',
    log_communication: false,
    secret: false,
    claude_code_usage_display: false,
    send_usage_limits: false,
    codex_model_switch_prompt: false,
    is_chat_session: false,
    deletable: true,
    ...overrides,
  } as PortalToken;
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderTokenList(
    opts: { groups?: VendorAccountGroup[]; tokens?: PortalToken[]; flag?: boolean } = {},
  ) {
    const created: CreateTokenRequest[] = [];
    const updated: UpdateTokenRequest[] = [];
    const fakeApi = {
      createToken: vi.fn(async (body: CreateTokenRequest) => {
        created.push(body);
        return { token: makeToken({ id: 'tok_new', name: body.name }), secret: 's' };
      }),
      updateToken: vi.fn(async (id: string, body: UpdateTokenRequest) => {
        updated.push(body);
        return makeToken({ id });
      }),
      deleteToken: vi.fn(),
      rotateToken: vi.fn(),
      myProjects: vi.fn(async () => []),
      serverModels: vi.fn(async () => []),
      updateChatSettings: vi.fn(),
      vendorAccounts: vi.fn(async () => ({ data: [] })),
      vendorAccountGroups: vi.fn(async () => ({ groups: opts.groups ?? [makeGroup()] })),
    };
    render(
      <ToastProvider>
        <TokenList
          t={t}
          api={fakeApi}
          tokens={opts.tokens ?? []}
          setTokens={vi.fn()}
          role="admin"
          models={[]}
          vendorAccountsEnabled={opts.flag ?? true}
        />
      </ToastProvider>,
    );
    return { fakeApi, created, updated };
  }

  describe(`TokenList vendor groups [${locale}]`, () => {
    it('enables the nested "incl. groups" checkbox only while "all" is ticked and clears it on untick', async () => {
      renderTokenList();
      fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
      await screen.findByRole('checkbox', { name: 'Claude pool' });

      const nested = screen.getByRole('checkbox', { name: t.tokenVendorAccessIncludeGroups });
      const all = screen.getByRole('checkbox', { name: t.tokenVendorAccessAll });
      expect(nested).toBeDisabled();

      fireEvent.click(all);
      expect(nested).toBeEnabled();
      fireEvent.click(nested);
      expect(nested).toBeChecked();

      fireEvent.click(all);
      expect(nested).toBeDisabled();
      expect(nested).not.toBeChecked();
    });

    it('sends include_groups only together with all', async () => {
      const { fakeApi, created } = renderTokenList();
      fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
      await screen.findByRole('checkbox', { name: 'Claude pool' });
      fireEvent.click(screen.getByRole('checkbox', { name: t.tokenVendorAccessAll }));
      fireEvent.click(screen.getByRole('checkbox', { name: t.tokenVendorAccessIncludeGroups }));
      fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'All' } });
      fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
      await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
      expect(created[0].vendor_access).toEqual({ all: true, include_groups: true });
    });

    it('sends a ticked group with an enabled empty prefix override', async () => {
      const { fakeApi, created } = renderTokenList();
      fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
      fireEvent.click(await screen.findByRole('checkbox', { name: 'Claude pool' }));

      const group = screen.getByRole('group', { name: 'Claude pool' });
      fireEvent.click(
        within(group).getByRole('checkbox', { name: t.tokenVendorAccessPrefixToggle }),
      );
      fireEvent.change(screen.getByLabelText(t.tokenNameLabel), { target: { value: 'G' } });
      fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
      await waitFor(() => expect(fakeApi.createToken).toHaveBeenCalled());
      expect(created[0].vendor_access).toEqual({
        all: false,
        accounts: [],
        groups: [{ group_id: 'vag_1', prefix_override: { enabled: true, value: '' } }],
      });
    });

    it('pre-ticks the groups a stored token names when editing', async () => {
      const token = makeToken({
        vendor_access: {
          all: true,
          include_groups: true,
          groups: [{ group_id: 'vag_1', prefix_override: { enabled: true, value: 'x/' } }],
        },
      });
      renderTokenList({ tokens: [token] });
      fireEvent.click(screen.getByRole('button', { name: t.tokenActionEdit }));
      expect(await screen.findByRole('checkbox', { name: 'Claude pool' })).toBeChecked();
      expect(
        screen.getByRole('checkbox', { name: t.tokenVendorAccessIncludeGroups }),
      ).toBeChecked();
      const group = screen.getByRole('group', { name: 'Claude pool' });
      expect(within(group).getByLabelText(t.tokenVendorAccessPrefixLabel)).toHaveValue('x/');
    });

    it('drops a stored group that no longer exists from the request', async () => {
      const token = makeToken({
        vendor_access: { all: false, groups: [{ group_id: 'vag_gone' }] },
      });
      const { fakeApi, updated } = renderTokenList({ tokens: [token] });
      fireEvent.click(screen.getByRole('button', { name: t.tokenActionEdit }));
      await screen.findByRole('checkbox', { name: 'Claude pool' });
      fireEvent.click(screen.getByRole('button', { name: t.tokenActionSave }));
      await waitFor(() => expect(fakeApi.updateToken).toHaveBeenCalled());
      expect(updated[0].vendor_access).toEqual({ all: false, accounts: [] });
    });

    it('does not fetch or show groups while the flag is off', () => {
      const { fakeApi } = renderTokenList({ flag: false });
      fireEvent.click(screen.getByRole('button', { name: t.tokenCreate }));
      expect(fakeApi.vendorAccountGroups).not.toHaveBeenCalled();
      expect(screen.queryByRole('checkbox', { name: 'Claude pool' })).not.toBeInTheDocument();
    });
  });
}
