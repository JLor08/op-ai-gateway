// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ColumnMenu } from './ColumnMenu';
import { ACTIVITY_COLUMNS, DEFAULT_HIDDEN_COLUMNS } from './activityColumns';
import { messages } from '../i18n';

const t = messages.de;

function renderMenu(overrides: Partial<React.ComponentProps<typeof ColumnMenu>> = {}) {
  const onToggle = vi.fn();
  const onOwnerDisplayChange = vi.fn();
  const onClose = vi.fn();
  const onReorder = vi.fn();
  const onReset = vi.fn();
  const onTimeDisplayChange = vi.fn();
  render(
    <ColumnMenu
      t={t}
      open
      anchorEl={document.body}
      onClose={onClose}
      columns={ACTIVITY_COLUMNS}
      hidden={[...DEFAULT_HIDDEN_COLUMNS]}
      onToggle={onToggle}
      onReorder={onReorder}
      onReset={onReset}
      scope="own"
      ownerDisplay="name"
      onOwnerDisplayChange={onOwnerDisplayChange}
      timeDisplay="absolute"
      onTimeDisplayChange={onTimeDisplayChange}
      moveLeftLabel={t.listColumnMoveLeft}
      moveRightLabel={t.listColumnMoveRight}
      {...overrides}
    />,
  );
  return { onToggle, onOwnerDisplayChange, onClose, onReorder, onReset, onTimeDisplayChange };
}

afterEach(cleanup);

describe('ColumnMenu', () => {
  it('checks default-visible columns and unchecks hidden ones', () => {
    renderMenu();
    expect(screen.getByRole('checkbox', { name: t.tableModel })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: t.activityColInput })).not.toBeChecked();
  });

  it('emits the column id when a checkbox is toggled', () => {
    const { onToggle } = renderMenu();
    fireEvent.click(screen.getByRole('checkbox', { name: t.activityColInput }));
    expect(onToggle).toHaveBeenCalledWith('input_tokens');
  });

  it('hides the owner display toggle in own-scope', () => {
    renderMenu({ scope: 'own' });
    expect(screen.queryByRole('radio', { name: t.activityOwnerId })).not.toBeInTheDocument();
  });

  it('shows the owner display toggle in all-scope and emits the choice', () => {
    const { onOwnerDisplayChange } = renderMenu({ scope: 'all' });
    expect(screen.getByRole('radio', { name: t.activityOwnerName })).toBeChecked();
    fireEvent.click(screen.getByRole('radio', { name: t.activityOwnerId }));
    expect(onOwnerDisplayChange).toHaveBeenCalledWith('id');
  });

  it('reorders a column before its predecessor when the move-left button is clicked', () => {
    const { onReorder } = renderMenu();
    // Second column (owner) can move before the first column (created_at).
    const owner = ACTIVITY_COLUMNS[1];
    const first = ACTIVITY_COLUMNS[0];
    fireEvent.click(
      screen.getByRole('button', { name: `${t.listColumnMoveLeft}: ${t[owner.labelKey]}` }),
    );
    expect(onReorder).toHaveBeenCalledWith(owner.id, first.id, 'before');
  });

  it('disables the move-left button on the first column and move-right on the last', () => {
    renderMenu();
    const firstLabel = t[ACTIVITY_COLUMNS[0].labelKey];
    const lastLabel = t[ACTIVITY_COLUMNS[ACTIVITY_COLUMNS.length - 1].labelKey];
    expect(
      screen.getByRole('button', { name: `${t.listColumnMoveLeft}: ${firstLabel}` }),
    ).toBeDisabled();
    expect(
      screen.getByRole('button', { name: `${t.listColumnMoveRight}: ${lastLabel}` }),
    ).toBeDisabled();
  });

  it('resets to the default columns', () => {
    const { onReset } = renderMenu();
    fireEvent.click(screen.getByRole('button', { name: t.listColumnsReset }));
    expect(onReset).toHaveBeenCalled();
  });
});
