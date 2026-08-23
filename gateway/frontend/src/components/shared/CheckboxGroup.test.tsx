// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CheckboxGroup } from './CheckboxGroup';

afterEach(cleanup);

describe('CheckboxGroup', () => {
  it('renders a group named by its legend and reports toggles', () => {
    const onToggle = vi.fn();
    render(
      <CheckboxGroup
        legend="Scopes"
        options={[
          { value: 'gateway:use', label: 'gateway:use' },
          { value: 'admin', label: 'admin' },
        ]}
        selected={['gateway:use']}
        onToggle={onToggle}
      />,
    );
    const group = screen.getByRole('group', { name: 'Scopes' });
    expect(within(group).getAllByRole('checkbox')[0]).toBeChecked();
    fireEvent.click(within(group).getByLabelText('admin'));
    expect(onToggle).toHaveBeenCalledWith('admin');
  });
  it("uses ariaLabel as the group's accessible name when provided", () => {
    render(
      <CheckboxGroup
        legend="Besitzer"
        ariaLabel="Besitzer GPU 1"
        options={[{ value: 'u1', label: 'Alice' }]}
        selected={[]}
        onToggle={() => {}}
      />,
    );
    expect(screen.getByRole('group', { name: 'Besitzer GPU 1' })).toBeInTheDocument();
  });
});
