// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActivityTable } from './ActivityTable';
import { ACTIVITY_COLUMNS } from './activityColumns';
import { messages } from '../i18n';
import type { UsageEvent } from '../api';

const t = messages.de;
// Default-visible columns now INCLUDE the "owner" column (scope-gating happens in
// the Activity view, not in this table). Column visibility here is driven purely
// by which ColumnDefs are passed in.
const defaultColumns = ACTIVITY_COLUMNS.filter((c) => c.defaultVisible);
// An "own-scope"-style column set: the Activity view drops the owner column.
const ownColumns = defaultColumns.filter((c) => c.id !== 'owner');
// Include the (default-hidden) server_name column for the render smoke test.
const columnsWithServer = ACTIVITY_COLUMNS.filter(
  (c) => c.defaultVisible || c.id === 'server_name',
);

function makeRow(overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id: 'req_1',
    user_id: 'usr_1',
    token_id: 'tok_1',
    api_flavor: 'portal_chat',
    model: 'qwen-coder',
    provider: 'mock',
    host: 'mock-host',
    input_tokens: 2,
    output_tokens: 6,
    total_tokens: 8,
    latency_ms: 14,
    status: 'success',
    created_at: '2026-07-10T12:01:00Z',
    cached_tokens: 0,
    prompt_per_second: 12.5,
    tokens_per_second: 40,
    http_status: 200,
    content_type: 'application/json',
    req_path: '/v1/chat/completions',
    provider_model: 'qwen2.5',
    requested_model: 'qwen-coder',
    stream: true,
    token_name: 'Dev Token',
    server_name: 'GPU 1',
    ...overrides,
  } as UsageEvent;
}

function renderTable(overrides: Partial<React.ComponentProps<typeof ActivityTable>> = {}) {
  const onSort = vi.fn();
  const onPageChange = vi.fn();
  const onLimitChange = vi.fn();
  const onView = vi.fn();
  const props: React.ComponentProps<typeof ActivityTable> = {
    t,
    rows: [makeRow()],
    columns: defaultColumns,
    ownerDisplay: 'name',
    sort: 'created_at',
    order: 'desc',
    onSort,
    page: 1,
    limit: 25,
    total: 100,
    onPageChange,
    onLimitChange,
    isEmpty: false,
    emptyLabel: t.activityEmpty,
    onView,
    // Backend-driven search + per-column filters (all controlled, server-side).
    q: '',
    onQChange: vi.fn(),
    textFilters: {},
    onTextFilter: vi.fn(),
    filterStatus: '',
    onFilterStatus: vi.fn(),
    filterStream: '',
    onFilterStream: vi.fn(),
    numericFilters: {},
    onNumericFilter: vi.fn(),
    timeFrom: '',
    timeTo: '',
    onTimeFilter: vi.fn(),
    timeDisplay: 'absolute',
    costUnit: 'eur_cent',
    currencyFactor: 1,
    onOpenColumns: vi.fn(),
    onReorderColumn: vi.fn(),
    ...overrides,
  };
  render(<ActivityTable {...props} />);
  return { onSort, onPageChange, onLimitChange, onView };
}

afterEach(cleanup);

describe('ActivityTable', () => {
  it('renders the default column headers and a data row', () => {
    renderTable({
      columns: columnsWithServer,
      rows: [makeRow({ requested_model: 'gpt-oss-20b' })],
    });
    const table = screen.getByRole('table');
    // Headers may contain an inline filter icon, so match on the label substring.
    expect(
      within(table).getByRole('columnheader', { name: new RegExp(t.tableModel) }),
    ).toBeInTheDocument();
    expect(
      within(table).getByRole('columnheader', { name: new RegExp(t.tableRequestedModel) }),
    ).toBeInTheDocument();
    expect(
      within(table).getByRole('columnheader', { name: new RegExp(t.tableStatus) }),
    ).toBeInTheDocument();
    // The latency column relabelled from "Latenz" to "Dauer" (activityColDuration).
    expect(
      within(table).getByRole('columnheader', { name: new RegExp(t.activityColDuration) }),
    ).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: 'qwen-coder' })).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: 'gpt-oss-20b' })).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: 'GPU 1' })).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: '14 ms' })).toBeInTheDocument();
  });

  it('renders an em dash for a legacy row with no recorded requested_model', () => {
    renderTable({
      columns: columnsWithServer,
      rows: [makeRow({ requested_model: '' })],
    });
    const table = screen.getByRole('table');
    expect(within(table).getByRole('cell', { name: '—' })).toBeInTheDocument();
    // The empty requested_model must never fall back to displaying `model`: the
    // model cell text ('qwen-coder') should appear exactly once (its own column).
    expect(within(table).getAllByRole('cell', { name: 'qwen-coder' })).toHaveLength(1);
  });

  it('emits the column id from a sort-header click', () => {
    const { onSort } = renderTable();
    // The sort label button's accessible name is just the column label.
    fireEvent.click(screen.getByRole('button', { name: t.activityColDuration }));
    expect(onSort).toHaveBeenCalledWith('latency_ms');
  });

  it('reflects the active sort column/direction on the header', () => {
    renderTable({ sort: 'latency_ms', order: 'asc' });
    // MUI marks the active TableSortLabel with aria-sort on the header cell.
    const header = screen.getByRole('columnheader', { name: new RegExp(t.activityColDuration) });
    expect(header).toHaveAttribute('aria-sort', 'ascending');
  });

  it('maps MUI 0-based next-page to a 1-based page', () => {
    const { onPageChange } = renderTable();
    fireEvent.click(screen.getByRole('button', { name: t.activityNextPage }));
    expect(onPageChange).toHaveBeenCalledWith(2);
  });

  it('emits the new limit from the rows-per-page select', () => {
    const { onLimitChange } = renderTable();
    fireEvent.change(screen.getByLabelText(t.activityRowsPerPage), { target: { value: '50' } });
    expect(onLimitChange).toHaveBeenCalledWith(50);
  });

  it('shows the empty label as a single spanning row', () => {
    renderTable({ rows: [], isEmpty: true, emptyLabel: t.activityEmpty });
    expect(screen.getByRole('cell', { name: t.activityEmpty })).toBeInTheDocument();
  });

  it('colors a successful request (http 200) with the success chip', () => {
    renderTable({ rows: [makeRow({ status: 'success', http_status: 200 })] });
    expect(screen.getByText('200').closest('[data-status]')).toHaveAttribute(
      'data-status',
      'active',
    );
  });

  it('colors a mid-stream error (status=error, http 200) as an error via the predicate', () => {
    renderTable({ rows: [makeRow({ status: 'error', http_status: 200 })] });
    // combined predicate wins over the raw http_status: standby = error styling
    expect(screen.getByText('200').closest('[data-status]')).toHaveAttribute(
      'data-status',
      'standby',
    );
  });

  it('renders the owner column (as passed in) respecting the name display', () => {
    // Owner is a normal column now; the all-scope view includes it in `columns`.
    renderTable({
      columns: defaultColumns,
      ownerDisplay: 'name',
      rows: [makeRow({ user_id: 'usr_42', user_name: 'Alice Admin' })],
    });
    expect(
      screen.getByRole('columnheader', { name: new RegExp(t.activityColOwner) }),
    ).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'Alice Admin' })).toBeInTheDocument();
    // §6a: the visible label is the user label ("Benutzer"), not "Besitzer".
    expect(screen.getByRole('columnheader', { name: /Benutzer/ })).toBeInTheDocument();
    expect(screen.queryByRole('columnheader', { name: /Besitzer/ })).not.toBeInTheDocument();
  });

  it('omits the owner column when it is not in the column set (own-scope view)', () => {
    renderTable({ columns: ownColumns });
    expect(
      screen.queryByRole('columnheader', { name: new RegExp(t.activityColOwner) }),
    ).not.toBeInTheDocument();
  });

  it('shows the raw user_id when the owner display is id', () => {
    renderTable({
      columns: defaultColumns,
      ownerDisplay: 'id',
      rows: [makeRow({ user_id: 'usr_42', user_name: 'Alice Admin' })],
    });
    expect(screen.getByRole('cell', { name: 'usr_42' })).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'Alice Admin' })).not.toBeInTheDocument();
  });

  it('shows a View action only when the row has a capture and calls onView', () => {
    const { onView } = renderTable({ rows: [makeRow({ id: 'req_cap', has_capture: true })] });
    const view = screen.getByRole('button', { name: t.activityColView });
    fireEvent.click(view);
    expect(onView).toHaveBeenCalledWith(expect.objectContaining({ id: 'req_cap' }));
  });

  it('hides the View action when the row has no capture', () => {
    renderTable({ rows: [makeRow({ has_capture: false })] });
    expect(screen.queryByRole('button', { name: t.activityColView })).not.toBeInTheDocument();
    // the column header is still present
    expect(screen.getByRole('columnheader', { name: t.activityColView })).toBeInTheDocument();
  });

  it('renders a non-interactive lock indicator for a locked capture (no View, no click handler)', () => {
    const { onView } = renderTable({
      rows: [makeRow({ has_capture: false, capture_locked: true })],
    });
    // No View button for a locked (secret, not-owned) capture.
    expect(screen.queryByRole('button', { name: t.activityColView })).not.toBeInTheDocument();
    // The lock is a non-interactive element (not a button), labelled for a11y.
    const lock = screen.getByLabelText(t.captureLocked);
    expect(lock.tagName).not.toBe('BUTTON');
    expect(lock.closest('button')).toBeNull();
    // Clicking it must not open the dialog.
    fireEvent.click(lock);
    expect(onView).not.toHaveBeenCalled();
  });

  it('prefers the View button over the lock when a row is both viewable and (defensively) locked', () => {
    const { onView } = renderTable({
      rows: [makeRow({ has_capture: true, capture_locked: true })],
    });
    expect(screen.queryByLabelText(t.captureLocked)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    expect(onView).toHaveBeenCalled();
  });

  it('renders neither View nor lock when a row is neither viewable nor locked', () => {
    renderTable({ rows: [makeRow({ has_capture: false, capture_locked: false })] });
    expect(screen.queryByRole('button', { name: t.activityColView })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.captureLocked)).not.toBeInTheDocument();
  });

  it('shows the session label in the token column for a no-token (chat) row', () => {
    // token_id === "" marks a token-less chat request; the backend fills
    // token_name with the user's display name, which must NOT leak here.
    renderTable({ rows: [makeRow({ token_id: '', token_name: 'Alice Admin' })] });
    expect(screen.getByRole('cell', { name: t.activityActiveSession })).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'Alice Admin' })).not.toBeInTheDocument();
  });

  it('shows the raw token name in the token column for a real-token row', () => {
    renderTable({ rows: [makeRow({ token_id: 'tok_1', token_name: 'Dev Token' })] });
    expect(screen.getByRole('cell', { name: 'Dev Token' })).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: t.activityActiveSession })).not.toBeInTheDocument();
  });

  describe('session + agent columns', () => {
    const sessionColumns = ACTIVITY_COLUMNS.filter(
      (c) => c.defaultVisible || c.id === 'session' || c.id === 'agent_id',
    );

    it('renders the source chip + short id, with the full id in the title', () => {
      renderTable({
        columns: sessionColumns,
        rows: [makeRow({ session_id: 'thread_x', session_source: 'codex' })],
      });
      const span = screen.getByTitle('thread_x');
      expect(span.textContent).toContain('codex');
      expect(span.textContent).toContain('thread_x');
    });

    it('truncates a long session id in the cell but keeps the full id in the title', () => {
      const longId = 'thread_abcdefghijklmnop';
      renderTable({
        columns: sessionColumns,
        rows: [makeRow({ session_id: longId, session_source: 'claude-code' })],
      });
      const span = screen.getByTitle(longId);
      expect(span.textContent).toContain('…');
      expect(span.textContent).not.toContain(longId);
      expect(span.getAttribute('title')).toBe(longId);
    });

    it('renders an em dash in the session column for a row without a session', () => {
      renderTable({
        columns: sessionColumns,
        rows: [makeRow({ session_id: undefined, session_source: undefined, agent_id: undefined })],
      });
      // Both the session and agent columns render "—"; there are two such cells.
      expect(screen.getAllByRole('cell', { name: '—' }).length).toBeGreaterThanOrEqual(2);
    });

    it('renders the agent id when present', () => {
      renderTable({ columns: sessionColumns, rows: [makeRow({ agent_id: 'agent-7' })] });
      expect(screen.getByRole('cell', { name: 'agent-7' })).toBeInTheDocument();
    });

    it('exposes a text filter on both the session and agent column headers', () => {
      renderTable({ columns: sessionColumns });
      expect(
        screen.getByRole('button', { name: `${t.listFilter}: ${t.activityColSession}` }),
      ).toBeInTheDocument();
      expect(
        screen.getByRole('button', { name: `${t.listFilter}: ${t.activityColAgent}` }),
      ).toBeInTheDocument();
    });
  });

  describe('service column (Phase 1 service accounts)', () => {
    const serviceColumns = ACTIVITY_COLUMNS.filter(
      (c) => c.defaultVisible || c.id === 'service_name',
    );

    it('renders the service name when present', () => {
      renderTable({ columns: serviceColumns, rows: [makeRow({ service_name: 'Nightly Batch' })] });
      expect(screen.getByRole('cell', { name: 'Nightly Batch' })).toBeInTheDocument();
    });

    it('renders an em dash when the row carries no service attribution', () => {
      renderTable({ columns: serviceColumns, rows: [makeRow({ service_name: undefined })] });
      expect(screen.getByRole('cell', { name: '—' })).toBeInTheDocument();
    });

    it('exposes a text filter on the service column header', () => {
      renderTable({ columns: serviceColumns });
      expect(
        screen.getByRole('button', { name: `${t.listFilter}: ${t.activityColService}` }),
      ).toBeInTheDocument();
    });
  });

  describe('cost_eur column (currency-unit selector, T5)', () => {
    const costColumns = ACTIVITY_COLUMNS.filter((c) => c.defaultVisible || c.id === 'cost_eur');

    it('renders a sub-cent cost via formatCost in eur_cent (4 decimals, not rounded away)', () => {
      renderTable({
        columns: costColumns,
        rows: [makeRow({ cost_eur: 0.0003 })],
        costUnit: 'eur_cent',
        currencyFactor: 1,
      });
      expect(screen.getByRole('cell', { name: '0.0300 ct' })).toBeInTheDocument();
    });

    it('renders the cost in USD when costUnit is usd, converted by the given factor', () => {
      renderTable({
        columns: costColumns,
        rows: [makeRow({ cost_eur: 0.0003 })],
        costUnit: 'usd',
        currencyFactor: 2,
      });
      expect(screen.getByRole('cell', { name: '$ 0.0006' })).toBeInTheDocument();
    });

    it('renders an em dash for a zero cost', () => {
      renderTable({ columns: costColumns, rows: [makeRow({ cost_eur: 0 })] });
      expect(screen.getByRole('cell', { name: '—' })).toBeInTheDocument();
    });

    it('renders an em dash when cost_eur is undefined', () => {
      renderTable({ columns: costColumns, rows: [makeRow({ cost_eur: undefined })] });
      expect(screen.getByRole('cell', { name: '—' })).toBeInTheDocument();
    });

    it('exposes a numeric range filter on the column header', () => {
      renderTable({ columns: costColumns });
      expect(
        screen.getByRole('button', { name: `${t.listFilter}: ${t.activityColCostEur}` }),
      ).toBeInTheDocument();
    });
  });
});
