// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { DrilldownHeader } from './DrilldownHeader';
import type { Translation } from './types';

const t = { back: 'Zurueck' } as unknown as Translation;

describe('DrilldownHeader', () => {
  it('renders the title and a back button named t.back', () => {
    const onBack = vi.fn();
    render(<DrilldownHeader t={t} title="GPU 1" onBack={onBack} />);
    const title = screen.getByText('GPU 1');
    expect(title).toBeInTheDocument();
    expect(title.tagName).toBe('P');
    fireEvent.click(screen.getByRole('button', { name: 'Zurueck' }));
    expect(onBack).toHaveBeenCalled();
  });
});
