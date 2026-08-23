// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { LanguageMenu } from './LanguageMenu';
import { messages } from '../../i18n';

const t = messages.de;

describe('LanguageMenu', () => {
  it('shows the current locale and offers both languages', async () => {
    const onSelect = vi.fn();
    render(<LanguageMenu locale="de" onSelect={onSelect} t={t} />);
    const trigger = screen.getByRole('button', { name: t.language });
    expect(trigger).toHaveTextContent('DE');
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole('menuitem', { name: 'EN' }));
    expect(onSelect).toHaveBeenCalledWith('en');
  });

  it('marks the active locale as selected', async () => {
    render(<LanguageMenu locale="en" onSelect={vi.fn()} t={messages.en} />);
    fireEvent.click(screen.getByRole('button', { name: messages.en.language }));
    const active = await screen.findByRole('menuitem', { name: 'EN' });
    expect(active).toHaveAttribute('aria-selected', 'true');
  });
});
