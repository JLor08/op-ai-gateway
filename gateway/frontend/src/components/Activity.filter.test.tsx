// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { AdminUser, PortalToken, UsagePage, UsageStats } from '../api';

const t = messages.de;

function installStorage() {
  const store = new Map<string, string>();
  const storage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } satisfies Storage;
  vi.stubGlobal('localStorage', storage);
  return store;
}

const emptyHistogram = { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 };
function makeStats(): UsageStats {
  return {
    totals: {
      total_requests: 0,
      error_count: 0,
      cached_tokens: 0,
      cache_write_tokens: 0,
      input_tokens: 0,
      output_tokens: 0,
    },
    prompt_per_second: emptyHistogram,
    tokens_per_second: emptyHistogram,
  };
}
function makeEmptyPage(): UsagePage {
  return { data: [], page: 1, limit: 25, total: 0, total_pages: 0 };
}
function makeToken(over: Partial<PortalToken> = {}): PortalToken {
  return {
    id: 'tok_own',
    name: 'My Token',
    secret_prefix: 'opaigw_',
    status: 'active',
    scopes: ['gateway:use'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-07-11T00:00:00Z',
    model_override: '',
    log_communication: false,
    secret: false,
    is_chat_session: false,
    deletable: true,
    ...over,
  };
}
const chatPseudo = makeToken({
  id: 'chat-session',
  name: 'Chat',
  is_chat_session: true,
  deletable: false,
});
function makeUser(over: Partial<AdminUser> = {}): AdminUser {
  return {
    id: 'usr_42',
    email: 'alice@example.test',
    display_name: 'Alice Admin',
    role: 'user',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-07-11T00:00:00Z',
    totp_enabled: false,
    ...over,
  };
}

function makeApi() {
  const unsubscribe = vi.fn();
  const api = {
    activity: vi.fn(async () => makeEmptyPage()),
    activityStats: vi.fn(async () => makeStats()),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    subscribeActivity: vi.fn(() => unsubscribe),
    tokens: vi.fn(async () => ({ data: [makeToken(), chatPseudo] })),
    adminUsers: vi.fn(async () => ({ data: [makeUser()] })),
    userTokens: vi.fn(async () => ({
      data: [makeToken({ id: 'tok_target', name: 'Target Token' }), chatPseudo],
    })),
    // Not exercised by this suite (owner/token filter chips only).
    getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
    usageGroups: vi.fn(async () => ({ data: [], group_by: 'server' })),
    captureDetail: vi.fn(async () => ({
      id: '',
      api_flavor: '',
      http_status: 0,
      created_at: '',
      req_headers: {},
      req_body: '',
      resp_headers: {},
      resp_body: '',
      truncated: false,
      secret: false,
      can_toggle_secret: false,
    })),
    deleteCapture: vi.fn(async () => ({ ok: true })),
    setCaptureSecret: vi.fn(async () => ({ ok: true })),
  };
  return { api };
}

function renderActivity(role = 'user') {
  const { api } = makeApi();
  render(
    <ToastProvider>
      <Activity t={t} api={api} role={role} onUnauthorized={vi.fn()} />
    </ToastProvider>,
  );
  return { api };
}

// SelectField + SearchableSelect are both MUI comboboxes: open by mouseDown on the
// combobox (scoped by label), then click the option (rendered in a portal).
async function pick(comboLabel: string, optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}
function lastArg(fn: unknown) {
  return (fn as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0] as Record<string, unknown>;
}
function lastActiveArgs(fn: unknown) {
  return (fn as { mock: { calls: unknown[][] } }).mock.calls.at(-1)! as unknown[];
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Activity user/token filter', () => {
  it('shows only the token dropdown for a non-admin and fetches own tokens', async () => {
    installStorage();
    const { api } = renderActivity('user');

    expect(
      await screen.findByRole('combobox', { name: t.activityTokenFilterLabel }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: t.activityScopeLabel })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('combobox', { name: t.activityUserFilterLabel }),
    ).not.toBeInTheDocument();
    await waitFor(() => expect(api.tokens).toHaveBeenCalled());
    expect(api.userTokens).not.toHaveBeenCalled();
  });

  it('offers own/specific/all and reveals the user dropdown only for a specific user', async () => {
    installStorage();
    renderActivity('admin');
    await screen.findByRole('combobox', { name: t.activityScopeLabel });

    await pick(t.activityScopeLabel, t.activityScopeSpecificUser);
    expect(
      await screen.findByRole('combobox', { name: t.activityUserFilterLabel }),
    ).toBeInTheDocument();
    // No token dropdown until a user is chosen.
    expect(
      screen.queryByRole('combobox', { name: t.activityTokenFilterLabel }),
    ).not.toBeInTheDocument();
  });

  it("loads the selected user's tokens and threads user_id into all four fetches", async () => {
    installStorage();
    const { api } = renderActivity('admin');
    await pick(t.activityScopeLabel, t.activityScopeSpecificUser);
    await pick(t.activityUserFilterLabel, 'Alice Admin');

    await waitFor(() => expect(api.userTokens).toHaveBeenCalledWith('usr_42'));
    expect(
      await screen.findByRole('combobox', { name: t.activityTokenFilterLabel }),
    ).toBeInTheDocument();

    await waitFor(() => expect(lastArg(api.activity).user_id).toBe('usr_42'));
    expect(lastArg(api.activityStats).user_id).toBe('usr_42');
    expect(lastArg(api.usageTimeSeries).user_id).toBe('usr_42');
    expect(lastActiveArgs(api.activeRequests)[1]).toMatchObject({ user_id: 'usr_42' });
  });

  it('maps the chat-session option to token_id=__none__ across all four fetches', async () => {
    installStorage();
    const { api } = renderActivity('user');
    await screen.findByRole('combobox', { name: t.activityTokenFilterLabel });

    await pick(t.activityTokenFilterLabel, t.activityActiveSession);

    await waitFor(() => expect(lastArg(api.activity).token_id).toBe('__none__'));
    expect(lastArg(api.activityStats).token_id).toBe('__none__');
    expect(lastArg(api.usageTimeSeries).token_id).toBe('__none__');
    expect(lastActiveArgs(api.activeRequests)[1]).toMatchObject({ token_id: '__none__' });
  });

  it('sends the raw token id for a real token', async () => {
    installStorage();
    const { api } = renderActivity('user');
    await screen.findByRole('combobox', { name: t.activityTokenFilterLabel });

    await pick(t.activityTokenFilterLabel, 'My Token');

    await waitFor(() => expect(lastArg(api.activity).token_id).toBe('tok_own'));
  });

  it('clears a stale user/token selection on scope change so no foreign token leaks into the fetch', async () => {
    installStorage();
    const { api } = renderActivity('admin');
    // Specific user U1, then U1's token -> threaded into the fetch.
    await pick(t.activityScopeLabel, t.activityScopeSpecificUser);
    await pick(t.activityUserFilterLabel, 'Alice Admin');
    await screen.findByRole('combobox', { name: t.activityTokenFilterLabel });
    await pick(t.activityTokenFilterLabel, 'Target Token');
    await waitFor(() => expect(lastArg(api.activity).token_id).toBe('tok_target'));

    // Switch scope back to own: the U1 token (tok_target) must NOT survive —
    // otherwise the backend pins the admin's user_id + a foreign token_id and
    // every section silently renders empty.
    await pick(t.activityScopeLabel, t.activityScopeOwn);
    await waitFor(() => expect(lastArg(api.activity).token_id).toBeUndefined());
    expect(lastArg(api.activity).user_id).toBeUndefined();
  });
});
