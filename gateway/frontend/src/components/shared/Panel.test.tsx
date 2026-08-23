// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Panel } from './Panel';

describe('Panel', () => {
  it('renders a <section> labelled by its level-2 heading', () => {
    render(
      <Panel titleId="agent-panel" title="Server-Reporting-Agent">
        <p>body</p>
      </Panel>,
    );
    const heading = screen.getByRole('heading', { name: 'Server-Reporting-Agent', level: 2 });
    expect(heading).toHaveAttribute('id', 'agent-panel');
    expect(screen.getByRole('region', { name: 'Server-Reporting-Agent' })).toBe(
      heading.closest('section'),
    );
  });
});
