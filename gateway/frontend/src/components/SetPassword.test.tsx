// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SetPassword } from './SetPassword';
import { ThemeControlsContext, type ThemeControls } from '../theme/useThemeControls';
import { messages } from '../i18n';
import type { PortalApi } from './shared/types';

const t = messages.de;
afterEach(cleanup);
const themeControls = {
  brand: { mark: { type: 'text', text: 'OP' }, title: 'AI Gateway' },
  productName: 'OP',
} as unknown as ThemeControls;

function renderSP(api: Partial<Pick<PortalApi, 'login' | 'setPassword'>>) {
  render(
    <ThemeControlsContext.Provider value={themeControls}>
      <SetPassword
        t={t}
        api={api as Pick<PortalApi, 'login' | 'setPassword'>}
        token="tok"
        locale="de"
        onSelectLocale={vi.fn()}
      />
    </ThemeControlsContext.Provider>,
  );
  fireEvent.change(screen.getByLabelText(t.newPasswordLabel), {
    target: { value: 'supersecret1' },
  });
  fireEvent.change(screen.getByLabelText(t.confirmPasswordLabel), {
    target: { value: 'supersecret1' },
  });
}

describe('SetPassword TOTP enrollment', () => {
  it('shows the enrollment step then confirms via login', async () => {
    const setPassword = vi.fn(async () => ({
      totp_enrollment_required: true as const,
      email: 'e@x.io',
      secret_base32: 'SEKRET',
      otpauth_uri: 'otpauth://x',
      qr_png_data_uri: 'data:image/png;base64,AAAA',
    }));
    const login = vi.fn(async () => ({
      id: 'u',
      email: 'e@x.io',
      display_name: 'E',
      role: 'user',
      preferred_language: 'de',
      totp_enabled: true,
      totp_mode: 'required',
      system_admin_mode: false,
      system_admin_mode_expires_at: '',
      system_admin_mode_require_password: true,
    }));
    renderSP({ setPassword, login });
    fireEvent.click(screen.getByRole('button', { name: t.setPasswordButton }));
    expect(await screen.findByAltText(t.totpQrAlt)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(t.totpCodeLabel), { target: { value: '123456' } });
    fireEvent.click(screen.getByRole('button', { name: t.loginVerifyButton }));
    await waitFor(() => expect(login).toHaveBeenCalledWith('e@x.io', 'supersecret1', '123456'));
    expect(await screen.findByText(t.setPasswordSuccess)).toBeInTheDocument();
  });
});
