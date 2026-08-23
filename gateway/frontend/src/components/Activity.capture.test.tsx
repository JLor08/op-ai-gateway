// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import {
  PortalApiError,
  type CaptureDetail,
  type UsageEvent,
  type UsagePage,
  type UsageStats,
} from '../api';
import type { PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';

const t = messages.de;

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
    stream: true,
    token_name: 'Dev Token',
    server_name: 'GPU 1',
    has_capture: true,
    ...overrides,
  } as UsageEvent;
}

function makePage(rows: UsageEvent[]): UsagePage {
  return { data: rows, page: 1, limit: 25, total: rows.length, total_pages: 1 };
}

const emptyHistogram = { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 };

function makeStats(): UsageStats {
  return {
    totals: {
      total_requests: 1,
      error_count: 0,
      cached_tokens: 0,
      cache_write_tokens: 0,
      input_tokens: 2,
      output_tokens: 6,
    },
    prompt_per_second: emptyHistogram,
    tokens_per_second: emptyHistogram,
  };
}

function makeApi(
  over: {
    activity?: PortalApi['activity'];
    captureDetail?: PortalApi['captureDetail'];
    deleteCapture?: PortalApi['deleteCapture'];
    setCaptureSecret?: PortalApi['setCaptureSecret'];
  } = {},
) {
  const unsubscribe = vi.fn();
  const detail: CaptureDetail = {
    id: 'req_1',
    api_flavor: 'portal_chat',
    http_status: 200,
    created_at: '2026-07-10T12:00:00Z',
    req_headers: { 'Content-Type': ['application/json'] },
    req_body: `{"model":"m","message":"hi"}`,
    resp_headers: { 'Content-Type': ['application/json'] },
    resp_body: `{"message":{"role":"assistant","content":"hello there"}}`,
    truncated: false,
    secret: false,
    can_toggle_secret: false,
  };
  const api = {
    activity: over.activity ?? vi.fn(async () => makePage([makeRow()])),
    activityStats: vi.fn(async () => makeStats()),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    subscribeActivity: vi.fn(() => unsubscribe),
    tokens: vi.fn(async () => ({ data: [] })),
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
    captureDetail: over.captureDetail ?? vi.fn(async () => detail),
    deleteCapture: over.deleteCapture ?? vi.fn(async () => ({ ok: true })),
    setCaptureSecret: over.setCaptureSecret ?? vi.fn(async () => ({ ok: true })),
    // Not exercised by this suite (capture drill-down only).
    getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
    usageGroups: vi.fn(async () => ({ data: [], group_by: 'server' })),
  };
  return { api, unsubscribe };
}

function renderActivity(
  over: {
    captureDetail?: PortalApi['captureDetail'];
    deleteCapture?: PortalApi['deleteCapture'];
    setCaptureSecret?: PortalApi['setCaptureSecret'];
    activity?: PortalApi['activity'];
  } = {},
  onUnauthorized = vi.fn(),
) {
  const { api } = makeApi(over);
  render(
    <ToastProvider>
      <Activity t={t} api={api} role="user" onUnauthorized={onUnauthorized} />
    </ToastProvider>,
  );
  return { api, onUnauthorized };
}

afterEach(cleanup);

describe('Activity capture view', () => {
  it('opens the dialog and shows the chat-derived response on View', async () => {
    const { api } = renderActivity();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    expect(api.captureDetail).toHaveBeenCalledWith('req_1');
    expect(await screen.findByText(t.captureDialogTitle)).toBeInTheDocument();
    expect(await screen.findByText('hello there')).toBeInTheDocument();
  });

  it('closes the dialog on the close button', async () => {
    renderActivity();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);
    fireEvent.click(screen.getByRole('button', { name: t.captureClose }));
    await waitFor(() => expect(screen.queryByText(t.captureDialogTitle)).not.toBeInTheDocument());
  });

  it('routes a 401 from the capture detail to onUnauthorized', async () => {
    const captureDetail = vi.fn(async () => {
      throw new PortalApiError(401, 'auth.session_invalid', 'expired');
    });
    const onUnauthorized = vi.fn();
    renderActivity({ captureDetail }, onUnauthorized);
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await waitFor(() => expect(onUnauthorized).toHaveBeenCalled());
  });

  it('deletes the capture on confirm: calls the API, closes both dialogs, refetches, and hides the View action once has_capture flips false', async () => {
    // The initial load returns has_capture: true (the row has a View button);
    // the silent refetch after delete must return has_capture: false, proving
    // the View action disappears once the server-side flag flips — not just
    // that a second activity() call happened.
    let calls = 0;
    const activity = vi.fn(async () => {
      calls += 1;
      return makePage([{ ...makeRow(), has_capture: calls === 1 }]);
    });
    const { api } = renderActivity({ activity });
    await screen.findByRole('cell', { name: 'qwen-coder' });
    expect(screen.getByRole('button', { name: t.activityColView })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);

    fireEvent.click(screen.getByRole('button', { name: t.captureDelete }));
    await screen.findByText(t.captureDeleteConfirm);
    // The CaptureDialog's own Delete button is still mounted but aria-hidden
    // behind the modal ConfirmDialog, so getAllByRole (hidden excluded) returns
    // only the ConfirmDialog's confirm action at index [0].
    fireEvent.click(screen.getAllByRole('button', { name: t.captureDelete })[0]);

    await waitFor(() => expect(api.deleteCapture).toHaveBeenCalledWith('req_1'));
    await waitFor(() => expect(screen.queryByText(t.captureDialogTitle)).not.toBeInTheDocument());
    expect(screen.queryByText(t.captureDeleteConfirm)).not.toBeInTheDocument();
    // Silent refetch after delete: a second activity() call beyond the initial load.
    await waitFor(() => expect(api.activity).toHaveBeenCalledTimes(2));
    // has_capture is now false for the row -> the View action is gone.
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: t.activityColView })).not.toBeInTheDocument(),
    );
  });

  it('keeps the capture open when the delete confirm is cancelled', async () => {
    const { api } = renderActivity();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);

    fireEvent.click(screen.getByRole('button', { name: t.captureDelete }));
    await screen.findByText(t.captureDeleteConfirm);
    fireEvent.click(screen.getByRole('button', { name: t.tokenActionCancel }));

    await waitFor(() => expect(screen.queryByText(t.captureDeleteConfirm)).not.toBeInTheDocument());
    expect(screen.getByText(t.captureDialogTitle)).toBeInTheDocument();
    expect(api.deleteCapture).not.toHaveBeenCalled();
  });

  it('surfaces a non-401 delete failure via the toast (above the still-open dialogs), not the hidden page Alert', async () => {
    // A destructive action must not fail silently. On a non-401 delete error
    // (e.g. 404 already-pruned, or a 500 store error) both the Capture and the
    // Confirm modal stay open; a page-level Alert would render behind their
    // backdrops and be invisible. The fix mirrors every other ConfirmDialog
    // delete flow (TokenList/ServerList/Mapping/Application) and surfaces the
    // error through the app-wide toast Snackbar, which renders above modals.
    const err = new PortalApiError(500, 'request.failed', 'internal');
    const deleteCapture = vi.fn(async () => {
      throw err;
    });
    const { api } = renderActivity({ deleteCapture });
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);

    fireEvent.click(screen.getByRole('button', { name: t.captureDelete }));
    await screen.findByText(t.captureDeleteConfirm);
    fireEvent.click(screen.getAllByRole('button', { name: t.captureDelete })[0]);

    await waitFor(() => expect(api.deleteCapture).toHaveBeenCalledWith('req_1'));

    // Error surfaced via the toast: formatPortalError -> "<code>: <label>".
    // The buggy path used the page Alert, which wraps the raw message as
    // "<portalError>: internal" and never renders this string.
    await screen.findByText(formatPortalError(err, t));
    // The raw API message ("internal") only ever appeared in the page-level
    // Alert; the toast idiom must not show it.
    expect(screen.queryByText('internal')).not.toBeInTheDocument();

    // Destructive-action safety: the capture is NOT treated as gone. Both
    // dialogs stay open and no silent refetch fires (only the initial load ran).
    expect(screen.getByText(t.captureDialogTitle)).toBeInTheDocument();
    expect(screen.getByText(t.captureDeleteConfirm)).toBeInTheDocument();
    expect(api.activity).toHaveBeenCalledTimes(1);
  });

  it('routes a 401 from delete to onUnauthorized', async () => {
    const deleteCapture = vi.fn(async () => {
      throw new PortalApiError(401, 'auth.session_invalid', 'expired');
    });
    const onUnauthorized = vi.fn();
    renderActivity({ deleteCapture }, onUnauthorized);
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);

    fireEvent.click(screen.getByRole('button', { name: t.captureDelete }));
    await screen.findByText(t.captureDeleteConfirm);
    fireEvent.click(screen.getAllByRole('button', { name: t.captureDelete })[0]);

    await waitFor(() => expect(onUnauthorized).toHaveBeenCalled());
  });

  it('toggles a capture secret from the dialog: calls setCaptureSecret, closes the dialog, and silently refetches', async () => {
    // Owner detail -> the dialog renders the secret toggle. The toggle logic lives
    // in Activity (mirrors removeCapture): call the API, close the dialog, silent
    // refetch (presence flips has_capture/capture_locked).
    let calls = 0;
    const activity = vi.fn(async () => {
      calls += 1;
      return makePage([makeRow()]);
    });
    const captureDetail = vi.fn(async () => ({
      id: 'req_1',
      api_flavor: 'portal_chat',
      http_status: 200,
      created_at: '2026-07-10T12:00:00Z',
      req_headers: {},
      req_body: '{}',
      resp_headers: {},
      resp_body: '{}',
      truncated: false,
      secret: false,
      can_toggle_secret: true,
    })) as unknown as PortalApi['captureDetail'];
    const setCaptureSecret = vi.fn(async () => ({ ok: true }));
    const { api } = renderActivity({ activity, captureDetail, setCaptureSecret });
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);

    fireEvent.click(screen.getByRole('button', { name: t.captureMarkSecret }));

    await waitFor(() => expect(api.setCaptureSecret).toHaveBeenCalledWith('req_1', true));
    await waitFor(() => expect(screen.queryByText(t.captureDialogTitle)).not.toBeInTheDocument());
    // Silent refetch after toggle: a second activity() call beyond the initial load.
    await waitFor(() => expect(api.activity).toHaveBeenCalledTimes(2));
    expect(calls).toBeGreaterThanOrEqual(2);
  });

  it('routes a 401 from setCaptureSecret to onUnauthorized', async () => {
    const captureDetail = vi.fn(async () => ({
      id: 'req_1',
      api_flavor: 'portal_chat',
      http_status: 200,
      created_at: '2026-07-10T12:00:00Z',
      req_headers: {},
      req_body: '{}',
      resp_headers: {},
      resp_body: '{}',
      truncated: false,
      secret: false,
      can_toggle_secret: true,
    })) as unknown as PortalApi['captureDetail'];
    const setCaptureSecret = vi.fn(async () => {
      throw new PortalApiError(401, 'auth.session_invalid', 'expired');
    });
    const onUnauthorized = vi.fn();
    renderActivity({ captureDetail, setCaptureSecret }, onUnauthorized);
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColView }));
    await screen.findByText(t.captureDialogTitle);

    fireEvent.click(screen.getByRole('button', { name: t.captureMarkSecret }));

    await waitFor(() => expect(onUnauthorized).toHaveBeenCalled());
  });
});
