// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import EditIcon from '@mui/icons-material/Edit';
import { RowActionsCell } from './RowActionsCell';
import type { RowAction } from './RowActionsMenu';

// This project runs vitest without `globals`, so RTL never registers its
// auto-cleanup: without this, every test's DOM (an open Tooltip included)
// stays in the document and the next test's queries match the stale one.
afterEach(cleanup);

function actions(label: string, over: Partial<RowAction> = {}): RowAction[] {
  return [
    {
      key: 'restart',
      label,
      icon: <EditIcon fontSize="small" />,
      onClick: vi.fn(),
      ...over,
    },
  ];
}

describe('RowActionsCell', () => {
  // RowAction.title is documented as "why a disabled action is disabled", and
  // the menu path has always honoured it -- but the INLINE path silently
  // dropped it, so a greyed-out icon button had no explanation at all. Nothing
  // caught that, which is why the field could rot: this is the missing test.
  it('forwards a disabled action’s title to the inline icon button', async () => {
    render(
      <RowActionsCell
        actions={actions('Neu starten', { disabled: true, title: 'Kein Prozess läuft' })}
        menuLabel="Aktionen"
      />,
    );
    const button = screen.getByRole('button', { name: 'Neu starten' });
    expect(button).toBeDisabled();
    // Hover the wrapper: a disabled MUI button is pointer-inert.
    fireEvent.mouseOver(button.parentElement as HTMLElement);
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Kein Prozess läuft');
  });

  it('still honours the title on the menu path', async () => {
    render(
      <RowActionsCell
        actions={actions('Laden', { disabled: true, title: 'Bereits geladen' })}
        menuLabel="Aktionen"
        maxInline={0}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Aktionen' }));
    fireEvent.mouseOver(await screen.findByText('Laden'));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Bereits geladen');
  });
});
