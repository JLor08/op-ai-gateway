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

  it('marks a disabled action with aria-disabled — focusable, but the click is refused', () => {
    const onClick = vi.fn();
    render(<IconAction label="Löschen" icon={<EditIcon />} onClick={onClick} disabled />);
    const button = screen.getByRole('button', { name: 'Löschen' });
    // NOT a real `disabled` attribute: that would make the control unfocusable,
    // so a keyboard/screen-reader user could never reach the reason (issue #26).
    expect(button).not.toBeDisabled();
    expect(button).toHaveAttribute('aria-disabled', 'true');
    // The control is no longer inert, so its click must be refused here rather
    // than silently swallowed by the DOM.
    fireEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
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
    // The button is the tooltip anchor directly: aria-disabled leaves it
    // pointer-interactive, so no wrapper span is needed for hover to fire.
    fireEvent.mouseOver(button);
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Kein Prozess läuft');
  });

  it('shows the label on hover when no title is given', async () => {
    render(<IconAction label="Duplizieren" icon={<EditIcon />} onClick={vi.fn()} />);
    fireEvent.mouseOver(screen.getByRole('button', { name: 'Duplizieren' }));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Duplizieren');
  });

  // The whole point of issue #26: a disabled action's reason must be reachable
  // by keyboard and screen readers, not only on pointer hover. A real `disabled`
  // attribute makes the control unfocusable, so the reason -- the one place the
  // UI says WHY the action is refused -- is announced to no one. The reason is
  // wired PERSISTENTLY through aria-describedby (not only while the Tooltip is
  // open), so a screen reader announces it the instant the control takes focus,
  // which is the moment that matters -- a describeChild Tooltip alone associates
  // its text ~100ms later, after the hover-delay, which is too late.
  it('announces a disabled action’s reason via a persistent aria-describedby', () => {
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
    // Present at render, with no tooltip open and no focus event required.
    const describedById = button.getAttribute('aria-describedby');
    expect(describedById).toBeTruthy();
    const description = document.getElementById(describedById as string);
    expect(description).toHaveTextContent('Kein Prozess läuft');
    // The accessible NAME stays the label, not the reason (a description, not a
    // rename): name-based lookups elsewhere must keep resolving.
    expect(button).toHaveAccessibleName('Neu starten');
  });

  it('wires no description for an enabled action or one with no reason', () => {
    const { rerender } = render(
      <IconAction label="Duplizieren" icon={<EditIcon />} onClick={vi.fn()} />,
    );
    expect(screen.getByRole('button', { name: 'Duplizieren' })).not.toHaveAttribute(
      'aria-describedby',
    );
    // A disabled action with no reason to give also carries no description.
    rerender(<IconAction label="Duplizieren" icon={<EditIcon />} onClick={vi.fn()} disabled />);
    expect(screen.getByRole('button', { name: 'Duplizieren' })).not.toHaveAttribute(
      'aria-describedby',
    );
  });
});
