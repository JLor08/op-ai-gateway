// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import EditIcon from '@mui/icons-material/Edit';
import { IconAction } from './IconAction';

// This project runs vitest without `globals`, so RTL never registers its
// auto-cleanup: without this, every test's DOM (an open Tooltip included)
// stays in the document and the next test's queries match the stale one.
afterEach(cleanup);

describe('IconAction', () => {
  it('exposes the label as its accessible name and fires onClick', () => {
    const onClick = vi.fn();
    render(<IconAction label="Bearbeiten" icon={<EditIcon />} onClick={onClick} />);
    const button = screen.getByRole('button', { name: 'Bearbeiten' });
    fireEvent.click(button);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('forwards the disabled state', () => {
    render(<IconAction label="Löschen" icon={<EditIcon />} onClick={vi.fn()} disabled />);
    expect(screen.getByRole('button', { name: 'Löschen' })).toBeDisabled();
  });

  // A disabled MUI IconButton sets `pointer-events: none`, so a Tooltip
  // anchored straight to it never fires -- which is exactly the case where the
  // hint matters, since `title` exists to say WHY an action is disabled. For
  // some tables (RuntimeAdminSection's live-status Restart) a disabled action
  // is the RESTING state, so every operator meets the unexplained grey button.
  it('shows a disabled action’s title on hover', async () => {
    render(
      <IconAction
        label="Neu starten"
        icon={<EditIcon />}
        onClick={vi.fn()}
        disabled
        title="Kein Prozess läuft"
      />,
    );
    const button = screen.getByRole('button', { name: 'Neu starten' });
    expect(button).toBeDisabled();
    // The wrapper span, not the button: the button itself is pointer-inert.
    fireEvent.mouseOver(button.parentElement as HTMLElement);
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Kein Prozess läuft');
  });

  it('shows the label on hover when no title is given', async () => {
    render(<IconAction label="Duplizieren" icon={<EditIcon />} onClick={vi.fn()} />);
    fireEvent.mouseOver(screen.getByRole('button', { name: 'Duplizieren' }));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Duplizieren');
  });
});
