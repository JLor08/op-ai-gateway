// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ManagementView } from './ManagementView';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { CurrentUser } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;
afterEach(cleanup);

function makeUser(over: Partial<CurrentUser> = {}): CurrentUser {
  return {
    id: 'u',
    email: 'e@x.io',
    display_name: 'E',
    role: 'user',
    preferred_language: 'de',
    totp_enabled: false,
    totp_mode: 'optional',
    system_admin_mode: false,
    system_admin_mode_expires_at: '',
    system_admin_mode_require_password: true,
    ...over,
  };
}
type ManagementApi = Pick<
  PortalApi,
  'changePassword' | 'me' | 'totpConfirm' | 'totpDisable' | 'totpEnroll'
>;

function renderView(api: Partial<ManagementApi>) {
  const fakeApi: ManagementApi = {
    changePassword: vi.fn(),
    me: vi.fn(),
    totpConfirm: vi.fn(),
    totpDisable: vi.fn(),
    totpEnroll: vi.fn(),
    ...api,
  };
  render(
    <ToastProvider>
      <ManagementView t={t} api={fakeApi} locale="de" onSelectLocale={vi.fn()} />
    </ToastProvider>,
  );
}

describe('ManagementView TOTP panel', () => {
  it('hides the panel when totp_mode is off', async () => {
    renderView({ me: vi.fn(async () => makeUser({ totp_mode: 'off' })) });
    await screen.findByRole('heading', { name: t.changePassword });
    expect(screen.queryByText(t.totpTitle)).toBeNull();
  });

  it('enrolls: shows QR + secret, confirms, becomes active', async () => {
    const me = vi.fn(async () => makeUser());
    const totpEnroll = vi.fn(async () => ({
      secret_base32: 'JBSWY3DPEHPK3PXP',
      otpauth_uri: 'otpauth://x',
      qr_png_data_uri: 'data:image/png;base64,AAAA',
    }));
    const totpConfirm = vi.fn(async () => makeUser({ totp_enabled: true }));
    renderView({ me, totpEnroll, totpConfirm });
    fireEvent.click(await screen.findByRole('button', { name: t.totpEnrollButton }));
    const img = (await screen.findByAltText(t.totpQrAlt)) as HTMLImageElement;
    expect(img.src).toContain('data:image/png;base64,AAAA');
    expect(screen.getByText('JBSWY3DPEHPK3PXP')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(t.totpCodeLabel), { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: t.totpConfirmButton }));
    await waitFor(() => expect(totpConfirm).toHaveBeenCalledWith('123456'));
    expect(await screen.findByText(t.totpStatusEnabled)).toBeInTheDocument();
  });

  it('shows Deaktivieren only in optional mode and disables with a code', async () => {
    const me = vi.fn(async () => makeUser({ totp_enabled: true, totp_mode: 'optional' }));
    const totpDisable = vi.fn(async () => ({ ok: true }));
    renderView({ me, totpDisable });
    fireEvent.change(await screen.findByLabelText(t.totpCodeLabel), {
      target: { value: '654321' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.totpDisableButton }));
    await waitFor(() => expect(totpDisable).toHaveBeenCalledWith('654321'));
  });

  it('hides Deaktivieren in required mode', async () => {
    renderView({ me: vi.fn(async () => makeUser({ totp_enabled: true, totp_mode: 'required' })) });
    await screen.findByText(t.totpStatusEnabled);
    expect(screen.queryByRole('button', { name: t.totpDisableButton })).toBeNull();
  });

  it('shows the panel with Deaktivieren for an already-enrolled user even when totp_mode is off, but hides the enroll button', async () => {
    const me = vi.fn(async () => makeUser({ totp_enabled: true, totp_mode: 'off' }));
    const totpDisable = vi.fn(async () => ({ ok: true }));
    renderView({ me, totpDisable });
    await screen.findByText(t.totpStatusEnabled);
    expect(screen.queryByRole('button', { name: t.totpEnrollButton })).toBeNull();
    fireEvent.change(await screen.findByLabelText(t.totpCodeLabel), {
      target: { value: '654321' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.totpDisableButton }));
    await waitFor(() => expect(totpDisable).toHaveBeenCalledWith('654321'));
  });

  it('keeps the panel hidden when totp_mode is off and the user is not enrolled', async () => {
    renderView({ me: vi.fn(async () => makeUser({ totp_enabled: false, totp_mode: 'off' })) });
    await screen.findByRole('heading', { name: t.changePassword });
    expect(screen.queryByText(t.totpTitle)).toBeNull();
  });
});
