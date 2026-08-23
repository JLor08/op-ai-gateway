// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActivityToolbar } from './ActivityToolbar';
import { messages } from '../i18n';

const t = messages.de;

function renderToolbar(overrides: Partial<React.ComponentProps<typeof ActivityToolbar>> = {}) {
  const handlers = {
    onRange: vi.fn(),
    onCustomFrom: vi.fn(),
    onCustomTo: vi.fn(),
    onGroupChain: vi.fn(),
    onRefresh: vi.fn(),
  };
  render(
    <ActivityToolbar
      t={t}
      range="30d"
      customFrom=""
      customTo=""
      groupChain={[]}
      {...handlers}
      {...overrides}
    />,
  );
  return handlers;
}

afterEach(cleanup);

describe('ActivityToolbar', () => {
  it('emits the range selection', async () => {
    const { onRange } = renderToolbar();
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.activityRangeLabel }));
    fireEvent.click(await screen.findByRole('option', { name: t.activityRange7d }));
    expect(onRange).toHaveBeenCalledWith('7d');
  });

  it('adds a group-by level via the chip builder (now in the Usage-metadata toolbar)', async () => {
    const { onGroupChain } = renderToolbar();
    fireEvent.click(screen.getByRole('button', { name: t.activityGroupAddLevel }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.activityGroupServer }));
    expect(onGroupChain).toHaveBeenCalledWith(['server']);
  });

  it('refreshes on the refresh button', () => {
    const { onRefresh } = renderToolbar();
    fireEvent.click(screen.getByRole('button', { name: t.activityRefresh }));
    expect(onRefresh).toHaveBeenCalled();
  });

  it('hides the custom from/to inputs for a preset range', () => {
    renderToolbar({ range: '30d' });
    expect(screen.queryByLabelText(t.activityRangeFrom)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.activityRangeTo)).not.toBeInTheDocument();
  });

  it('reveals two datetime-local inputs when the range is custom and emits edits', () => {
    const { onCustomFrom, onCustomTo } = renderToolbar({ range: 'custom' });
    const from = screen.getByLabelText(t.activityRangeFrom);
    const to = screen.getByLabelText(t.activityRangeTo);
    expect(from).toHaveAttribute('type', 'datetime-local');
    expect(to).toHaveAttribute('type', 'datetime-local');
    fireEvent.change(from, { target: { value: '2026-01-01T00:00' } });
    expect(onCustomFrom).toHaveBeenCalledWith('2026-01-01T00:00');
    fireEvent.change(to, { target: { value: '2026-02-01T00:00' } });
    expect(onCustomTo).toHaveBeenCalledWith('2026-02-01T00:00');
  });

  // The search box, per-column filters (status/model/server), the columns menu,
  // and the scope switch all moved out of the toolbar into the table / column
  // menu / top of the view. The toolbar must no longer render any of them.
  it('does not render the scope switch (it now lives at the top of the view)', () => {
    renderToolbar();
    expect(screen.queryByLabelText(t.activityScopeLabel)).not.toBeInTheDocument();
  });

  it('does not render the search box (it now lives in the table)', () => {
    renderToolbar();
    expect(screen.queryByLabelText(t.activitySearchLabel)).not.toBeInTheDocument();
  });

  it('does not render the status filter (per-column filters moved into the table)', () => {
    renderToolbar();
    expect(screen.queryByLabelText(t.activityFilterStatus)).not.toBeInTheDocument();
  });

  it('does not render the columns menu button (it now lives in the table)', () => {
    renderToolbar();
    expect(screen.queryByRole('button', { name: t.activityColumns })).not.toBeInTheDocument();
  });
});
