// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Login } from './Login';
import { ThemeControlsContext, type ThemeControls } from '../theme/useThemeControls';
import { messages } from '../i18n';
import type { PortalApi } from './shared/types';

const t = messages.de;
afterEach(cleanup);
const themeControls = {
  brand: { mark: { type: 'text', text: 'OP' }, title: 'AI Gateway' },
  productName: 'OP',
} as unknown as ThemeControls;

function renderLogin(api: Partial<Pick<PortalApi, 'login'>>, onSuccess = vi.fn()) {
  render(
    <ThemeControlsContext.Provider value={themeControls}>
      <Login
        t={t}
        api={api as Pick<PortalApi, 'login'>}
        locale="de"
        onSelectLocale={vi.fn()}
        onSuccess={onSuccess}
      />
    </ThemeControlsContext.Provider>,
  );
  fireEvent.change(screen.getByLabelText(t.emailLabel), { target: { value: 'e@x.io' } });
  fireEvent.change(screen.getByLabelText(t.passwordLabel), { target: { value: 'pw' } });
}

describe('Login TOTP', () => {
  it('prompts for a code on totp_required, then logs in with it', async () => {
    const login = vi.fn().mockResolvedValueOnce({ totp_required: true }).mockResolvedValueOnce({
      id: 'u',
      email: 'e@x.io',
      display_name: 'E',
      role: 'user',
      preferred_language: 'de',
      totp_enabled: true,
      totp_mode: 'optional',
    });
    const onSuccess = vi.fn();
    renderLogin({ login }, onSuccess);
    fireEvent.click(screen.getByRole('button', { name: t.loginButton }));
    const code = await screen.findByLabelText(t.totpCodeLabel);
    fireEvent.change(code, { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: t.loginVerifyButton }));
    await waitFor(() => expect(onSuccess).toHaveBeenCalled());
    expect(login).toHaveBeenNthCalledWith(2, 'e@x.io', 'pw', '123456');
  });

  it('shows the enrollment QR on totp_enrollment_required', async () => {
    const login = vi.fn().mockResolvedValueOnce({
      totp_enrollment_required: true,
      secret_base32: 'SEKRET',
      otpauth_uri: 'otpauth://x',
      qr_png_data_uri: 'data:image/png;base64,AAAA',
    });
    renderLogin({ login });
    fireEvent.click(screen.getByRole('button', { name: t.loginButton }));
    expect(await screen.findByAltText(t.totpQrAlt)).toBeInTheDocument();
    expect(screen.getByText('SEKRET')).toBeInTheDocument();
    expect(screen.getByLabelText(t.totpCodeLabel)).toBeInTheDocument();
  });
});
