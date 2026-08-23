// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { GroupByChainBuilder } from './GroupByChainBuilder';
import { messages } from '../i18n';

const t = messages.de;
afterEach(cleanup);

function renderBuilder(chain: string[]) {
  const onChange = vi.fn();
  render(<GroupByChainBuilder t={t} chain={chain} onChange={onChange} />);
  return onChange;
}

describe('GroupByChainBuilder', () => {
  it('renders a chip per dimension in chain order', () => {
    renderBuilder(['user', 'server', 'model']);
    // Chips carry the dimension labels; the leading label is activityGroupBy.
    expect(screen.getByText(t.activityGroupUser)).toBeInTheDocument();
    expect(screen.getByText(t.activityGroupServer)).toBeInTheDocument();
    expect(screen.getByText(t.activityGroupModel)).toBeInTheDocument();
  });

  it('adds a level via the + Ebene menu, excluding already-used dimensions', async () => {
    const onChange = renderBuilder(['user']);
    fireEvent.click(screen.getByRole('button', { name: t.activityGroupAddLevel }));
    // The menu must NOT offer "user" (already used).
    expect(screen.queryByRole('menuitem', { name: t.activityGroupUser })).not.toBeInTheDocument();
    fireEvent.click(await screen.findByRole('menuitem', { name: t.activityGroupServer }));
    expect(onChange).toHaveBeenCalledWith(['user', 'server']);
  });

  it('removes a level (incl. a middle one), closing the gap', () => {
    const onChange = renderBuilder(['user', 'server', 'model']);
    fireEvent.click(
      screen.getByRole('button', {
        name: `${t.activityGroupRemoveLevel}: ${t.activityGroupServer}`,
      }),
    );
    expect(onChange).toHaveBeenCalledWith(['user', 'model']);
  });

  it('hides the + Ebene button once all dimensions are used', () => {
    renderBuilder(['session', 'server', 'user', 'token', 'model', 'service', 'project']);
    expect(screen.queryByRole('button', { name: t.activityGroupAddLevel })).not.toBeInTheDocument();
  });

  it("offers 'service' (Phase 1 service accounts) as an addable dimension", async () => {
    const onChange = renderBuilder(['user']);
    fireEvent.click(screen.getByRole('button', { name: t.activityGroupAddLevel }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.activityGroupService }));
    expect(onChange).toHaveBeenCalledWith(['user', 'service']);
  });

  it("offers 'project' (spec: 2026-08-08-projects-design.md) as an addable dimension", async () => {
    const onChange = renderBuilder(['user']);
    fireEvent.click(screen.getByRole('button', { name: t.activityGroupAddLevel }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.activityGroupProject }));
    expect(onChange).toHaveBeenCalledWith(['user', 'project']);
  });

  it('clears the whole chain via the Leeren button', () => {
    const onChange = renderBuilder(['user', 'server', 'model']);
    fireEvent.click(screen.getByRole('button', { name: t.activityGroupClear }));
    expect(onChange).toHaveBeenCalledWith([]);
  });

  it('hides the Leeren button when the chain is empty', () => {
    renderBuilder([]);
    expect(screen.queryByRole('button', { name: t.activityGroupClear })).not.toBeInTheDocument();
  });

  it("reorders via drag (jsdom rect=0 -> place 'after')", () => {
    const onChange = renderBuilder(['user', 'server', 'model']);
    const dt = { setData: vi.fn(), effectAllowed: '', dropEffect: '' };
    const source = screen.getByText(t.activityGroupModel);
    const target = screen.getByText(t.activityGroupUser);
    fireEvent.dragStart(source, { dataTransfer: dt });
    fireEvent.dragOver(target, { dataTransfer: dt });
    fireEvent.drop(target, { dataTransfer: dt });
    // dragging "model" onto "user" with a zero-size rect resolves to place "after":
    // moveColumn(["user","server","model"], "model", "user", "after") = ["user","model","server"].
    expect(onChange).toHaveBeenCalledWith(['user', 'model', 'server']);
  });
});
