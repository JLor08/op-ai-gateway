// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach } from 'vitest';
import { useState } from 'react';
import { render, screen, cleanup, fireEvent, within } from '@testing-library/react';
import { OrderedMemberList } from './OrderedMemberList';
import { messages } from '../../i18n';
import type { ModelOption } from '../../api';

const t = messages.de;

afterEach(() => cleanup());

const available: ModelOption[] = [
  { id: 'm1', display_name: 'm1', flavors: ['openai'] },
  { id: 'm2', display_name: 'm2', flavors: ['openai'] },
  { id: 'grp', display_name: 'grp', flavors: ['openai'], is_group: true },
];

function Harness({ initial }: { initial: string[] }) {
  const [members, setMembers] = useState<string[]>(initial);
  return <OrderedMemberList members={members} onChange={setMembers} available={available} t={t} />;
}

function memberRows() {
  return screen.getAllByRole('listitem');
}

describe('OrderedMemberList', () => {
  it('moves a member down (reorders priority)', () => {
    render(<Harness initial={['m1', 'm2']} />);
    let rows = memberRows();
    expect(within(rows[0]).getByText('m1')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: `${t.modelGroupMoveDown}: m1` }));

    rows = memberRows();
    expect(within(rows[0]).getByText('m2')).toBeInTheDocument();
    expect(within(rows[1]).getByText('m1')).toBeInTheDocument();
  });

  it('moves a member up (reorders priority)', () => {
    render(<Harness initial={['m1', 'm2']} />);
    fireEvent.click(screen.getByRole('button', { name: `${t.modelGroupMoveUp}: m2` }));
    const rows = memberRows();
    expect(within(rows[0]).getByText('m2')).toBeInTheDocument();
  });

  it('disables move-up on the first row and move-down on the last row', () => {
    render(<Harness initial={['m1', 'm2']} />);
    expect(screen.getByRole('button', { name: `${t.modelGroupMoveUp}: m1` })).toBeDisabled();
    expect(screen.getByRole('button', { name: `${t.modelGroupMoveDown}: m2` })).toBeDisabled();
  });

  it('appends a picked model at the lowest priority', async () => {
    render(<Harness initial={['m1']} />);
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupAddMember }));
    fireEvent.click(await screen.findByRole('option', { name: 'm2' }));

    const rows = memberRows();
    expect(rows).toHaveLength(2);
    expect(within(rows[1]).getByText('m2')).toBeInTheDocument();
  });

  it('removes a member', () => {
    render(<Harness initial={['m1', 'm2']} />);
    fireEvent.click(screen.getByRole('button', { name: `${t.modelGroupRemoveMember}: m1` }));
    const rows = memberRows();
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByText('m2')).toBeInTheDocument();
  });

  it('does not offer an already-added member (dedup)', () => {
    render(<Harness initial={['m1']} />);
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupAddMember }));
    // m1 is already a member → not offered.
    expect(screen.queryByRole('option', { name: 'm1' })).toBeNull();
    // m2 is offerable.
    expect(screen.getByRole('option', { name: 'm2' })).toBeInTheDocument();
  });

  it('offers a group as a member option, labelled with the group chip suffix', () => {
    render(<Harness initial={['m1']} />);
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupAddMember }));
    expect(screen.getByRole('option', { name: `grp (${t.modelGroupChip})` })).toBeInTheDocument();
  });

  it('excludes the group being edited from its own add-member options (selfName)', () => {
    function SelfHarness() {
      const [members, setMembers] = useState<string[]>([]);
      return (
        <OrderedMemberList
          members={members}
          onChange={setMembers}
          available={available}
          t={t}
          selfName="grp"
        />
      );
    }
    render(<SelfHarness />);
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.modelGroupAddMember }));
    expect(screen.queryByRole('option', { name: `grp (${t.modelGroupChip})` })).toBeNull();
    expect(screen.getByRole('option', { name: 'm1' })).toBeInTheDocument();
  });
});
