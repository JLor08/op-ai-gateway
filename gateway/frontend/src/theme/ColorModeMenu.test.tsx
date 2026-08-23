// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ColorModeMenu } from './ColorModeMenu';
import { ThemeControlsContext, type ThemeControls } from './useThemeControls';
import { messages } from '../i18n';

const t = messages.de;

afterEach(() => {
  cleanup();
});

function renderWith(overrides: Partial<ThemeControls>) {
  const value: ThemeControls = {
    activeThemeId: 'default',
    setActiveThemeId: vi.fn(),
    reloadTheme: vi.fn(),
    pref: 'system',
    effective: 'light',
    setMode: vi.fn(),
    toggle: vi.fn(),
    hasDark: true,
    brand: { mark: { type: 'text', text: 'OP' }, title: 'AI Gateway' },
    productName: 'On-Prem AI Gateway',
    ...overrides,
  };
  render(
    <ThemeControlsContext.Provider value={value}>
      <ColorModeMenu t={t} />
    </ThemeControlsContext.Provider>,
  );
  return value;
}

describe('ColorModeMenu', () => {
  it('renders nothing when the theme has no dark variant', () => {
    renderWith({ hasDark: false });
    expect(screen.queryByRole('button', { name: t.colorMode })).not.toBeInTheDocument();
  });

  it('opens the menu and selects the dark mode', async () => {
    const value = renderWith({ pref: 'system' });
    fireEvent.click(screen.getByRole('button', { name: t.colorMode }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.colorModeDark }));
    expect(value.setMode).toHaveBeenCalledWith('dark');
  });

  it('marks the active mode as selected', async () => {
    renderWith({ pref: 'light' });
    fireEvent.click(screen.getByRole('button', { name: t.colorMode }));
    expect(await screen.findByRole('menuitem', { name: t.colorModeLight })).toHaveAttribute(
      'aria-selected',
      'true',
    );
  });
});
