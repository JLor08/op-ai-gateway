// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactElement } from 'react';
import App from './App';
import type { PortalApplication, PortalModelMapping, PortalServer, ServerStatus } from './api';
import { messages } from './i18n';
import { ThemeRoot } from './theme/ThemeRoot';

/**
 * The shell now calls `useThemeControls()` (via the color-mode toggle and the
 * System settings view), so every render must sit inside `ThemeRoot`. Wrap here
 * once rather than at each call site.
 */
function renderApp(ui: ReactElement = <App />) {
  return render(<ThemeRoot>{ui}</ThemeRoot>);
}

/**
 * This jsdom build exposes neither a working `localStorage` nor `matchMedia`,
 * which the color-mode hook reads. Install in-memory stubs so ThemeRoot mounts
 * deterministically in light mode. (Mirrors useColorMode.test.tsx / ThemeRoot.test.tsx.)
 */
function installColorModeStubs() {
  const store = new Map<string, string>();
  const storage = {
    getItem: (key: string) => (store.has(key) ? store.get(key)! : null),
    setItem: (key: string, value: string) => {
      store.set(key, String(value));
    },
    removeItem: (key: string) => {
      store.delete(key);
    },
    clear: () => {
      store.clear();
    },
    key: (index: number) => Array.from(store.keys())[index] ?? null,
    get length() {
      return store.size;
    },
  } satisfies Storage;
  vi.stubGlobal('localStorage', storage);
  const mql = {
    matches: false,
    media: '(prefers-color-scheme: dark)',
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  };
  vi.stubGlobal('matchMedia', vi.fn().mockReturnValue(mql));
  // Two EventSource consumers exist: Activity subscribes to
  // `/api/portal/usage/events` (a no-op here — SSE behavior is covered in
  // Activity.test.tsx), and the Chat store subscribes to a background run at
  // `/api/portal/chats/{id}/runs/{runId}/events`. For a run URL this mock
  // synthesizes the named snapshot/delta/done events per `chatStreamMode`, so
  // the chat tests drive the run-subscriber path without a real network.
  class MockEventSource {
    url: string;
    onopen: (() => void) | null = null;
    onerror: (() => void) | null = null;
    listeners: Record<string, ((e: MessageEvent) => void)[]> = {};
    constructor(url: string) {
      this.url = url;
      if (/\/runs\/[^/]+\/events$/.test(url)) {
        // Emit after the store has synchronously attached its listeners.
        setTimeout(() => this.playRun(), 0);
      }
    }
    addEventListener(type: string, fn: (e: MessageEvent) => void) {
      (this.listeners[type] ??= []).push(fn);
    }
    removeEventListener(type: string, fn: (e: MessageEvent) => void) {
      this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn);
    }
    emit(type: string, data: unknown) {
      for (const fn of this.listeners[type] ?? [])
        fn({ data: JSON.stringify(data) } as MessageEvent);
    }
    close() {}
    playRun() {
      const model = 'qwen-coder'; // default openai model in these tests
      if (chatStreamMode === 'reasoning') {
        this.emit('delta', { reasoning: 'Because reasons' });
        this.emit('delta', { content: `Answer for ${model}` });
        this.emit('done', {
          reasoning: 'Because reasons',
          content: `Answer for ${model}`,
          status: 'completed',
        });
      } else if (chatStreamMode === 'error') {
        this.emit('done', { content: '', status: 'error', error: 'boom' });
      } else if (chatStreamMode === 'abort') {
        this.emit('delta', { content: 'partial' });
        this.emit('done', { content: 'partial', status: 'canceled' });
      } else if (chatStreamMode === 'pending') {
        this.emit('delta', { content: 'partial-pending' });
        // Never emit `done` — models a long-running background run.
      } else {
        this.emit('delta', { content: `Mock stream for ${model}` });
        this.emit('done', { content: `Mock stream for ${model}`, status: 'completed' });
      }
    }
  }
  vi.stubGlobal('EventSource', MockEventSource);
  // The chat image attach path downscales via `new Image()`, which never fires
  // onload for a data URL under this jsdom build. Stub a decoder that reports
  // small dims (<= the 1568px cap) so prepareImageDataUrl resolves with the
  // original data URL (no canvas re-encode needed).
  class MockImage {
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    naturalWidth = 0;
    naturalHeight = 0;
    set src(_value: string) {
      queueMicrotask(() => {
        this.naturalWidth = 24;
        this.naturalHeight = 18;
        this.onload?.();
      });
    }
  }
  vi.stubGlobal('Image', MockImage);
}

const defaultTokenRows = [
  {
    id: 'tok_dev',
    name: 'Dev Token',
    secret_prefix: 'dev-secr',
    status: 'active',
    scopes: ['gateway:use', 'admin'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-07-10T12:00:00Z',
  },
];

const defaultUsageRows = [
  {
    id: 'req_api',
    user_id: 'usr_dev',
    token_id: 'tok_dev',
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
    // SP-A/SP-B enriched fields so the paged /api/portal/usage envelope carries
    // a full UsageEvent (Activity's default columns read server_name/token_name/
    // http_status; stats sums cached_tokens).
    cached_tokens: 0,
    prompt_per_second: 120.5,
    tokens_per_second: 42,
    http_status: 200,
    content_type: 'text/event-stream',
    req_path: '/v1/chat/completions',
    provider_model: 'qwen-coder',
    stream: true,
    token_name: 'Dev Token',
    server_name: 'GPU 1',
    route_id: 'route_mock_qwen',
  },
];

const defaultUserRows = [
  {
    id: 'usr_dev',
    email: 'dev@example.test',
    display_name: 'Dev User',
    role: 'admin',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-07-10T12:00:00Z',
  },
];

const defaultServerRows: PortalServer[] = [
  {
    id: 'srv_dev',
    name: 'GPU 1',
    domain: 'gpu1.example.test',
    server_path_suffix: '',
    status: 'active',
    health_status: 'healthy',
    owners: [{ id: 'usr_dev', email: 'dev@example.test', display_name: 'Dev User' }],
    last_seen_at: '2026-07-10T12:00:00Z',
    created_at: '2026-07-10T11:00:00Z',
    netbird_enabled: false,
    netbird_setup_key_id: '',
    netbird_group_id: '',
    netbird_peer_id: '',
    netbird_connected: false,
    netbird_group_ids: [],
    netbird_peer_managed: false,
    netbird_policy_override: '',
    netbird_allow_ping: false,
    netbird_ping_exclude: false,
    agent_status: 'unconfigured',
    agent_presence_timeout_seconds: 0,
    estimated_watts: 0,
    idle_watts: 0,
    price_per_kwh: 0,
    pue: 0,
    price_unit: 'eur_cent',
    admin_groups: [{ id: 'ag_default', name: 'Default Admin Group' }],
    system_group_id: 'sg_default',
    system_group_name: 'Default System Group',
  },
];

// The admin-tier group candidates GET /api/portal/server-admin-group-candidates
// offers by default (Phase B, spec 2026-08-10) -- a single group under a single
// system group, so the create form's picker auto-selects (no extra step) and
// the "Server erstellen" button stays enabled across the existing tests that
// don't care about admin-group linkage.
const defaultAdminGroupCandidates = [
  {
    id: 'ag_default',
    name: 'Default Admin Group',
    parent_group_id: 'sg_default',
    parent_group_name: 'Default System Group',
  },
];

let apiState: {
  tokenRows: typeof defaultTokenRows;
  usageRows: typeof defaultUsageRows;
  userRows: typeof defaultUserRows;
  serverRows: PortalServer[];
  applicationRows: PortalApplication[];
  mappingRows: PortalModelMapping[];
  agentTokenExists: Record<string, boolean>;
  chatRows: {
    id: string;
    title: string;
    created_at: string;
    updated_at: string;
    content: unknown;
  }[];
};
let loggedIn = true;
let currentRole = 'admin';
// System default language returned by GET /api/auth/session; a test-local
// variable (like loggedIn/currentRole) so a test can drive the login-screen
// locale. Reset to "de" before each test.
let sessionLanguage = 'de';
// Controls the shape of the mocked POST /v1/chat/completions SSE stream so a
// single global handler can serve the normal, reasoning, error-frame, and
// aborted-mid-stream scenarios. Reset to "normal" before each test.
let chatStreamMode: 'normal' | 'reasoning' | 'error' | 'abort' | 'pending' = 'normal';
// Drives GET /api/portal/netbird/enabled. Defaults false (matches the prior
// no-mock behavior, where the unhandled route 404'd and every consumer treated
// it as disabled) so pre-existing tests (incl. ServerList's own netbird-gated
// row actions/columns, which read `enabled` = fully configured) are unaffected;
// flipped true only by the NetBird-view nav tests below. Also drives
// `module_enabled` (the RAW enable checkbox) by default — App.tsx's nav gate
// reads `module_enabled`, not `enabled`, since the checkbox can be on before
// url/token are configured. `netbirdRawCheckboxMock` independently overrides
// `module_enabled` when a test needs the two to diverge (checkbox on, module
// not yet fully configured).
let netbirdModuleEnabledMock = false;
let netbirdRawCheckboxMock: boolean | null = null;
// Drives CurrentUser.system_admin_mode on the mocked /api/auth/session +
// /api/portal/me responses. A system_admin session starts NOT elevated
// (matches the backend default), so only tests that specifically need the
// elevated system-admin capability (System/NetBird/Logs nav + view, the
// system-admin role option in the user form) flip this true.
let systemAdminModeMock = false;

const devUser = {
  id: 'usr_dev',
  email: 'dev@example.test',
  display_name: 'Dev User',
  role: 'admin',
  preferred_language: 'de',
};

beforeEach(() => {
  installColorModeStubs();
  apiState = {
    tokenRows: defaultTokenRows.map((row) => ({ ...row, scopes: [...row.scopes] })),
    usageRows: defaultUsageRows.map((row) => ({ ...row })),
    userRows: defaultUserRows.map((row) => ({ ...row })),
    serverRows: defaultServerRows.map((row) => ({
      ...row,
      owners: row.owners.map((owner) => ({ ...owner })),
    })),
    applicationRows: [],
    mappingRows: [],
    agentTokenExists: {},
    chatRows: [],
  };
  loggedIn = true;
  currentRole = 'admin';
  sessionLanguage = 'de';
  chatStreamMode = 'normal';
  netbirdModuleEnabledMock = false;
  netbirdRawCheckboxMock = null;
  systemAdminModeMock = false;
  window.sessionStorage.clear();
  // The reworked Chat persists its transcript + settings to localStorage. jsdom
  // shares one window across a file, so a transcript persisted by an earlier
  // chat test could otherwise hydrate into a later one and break its empty
  // state. (This jsdom build does not expose window.localStorage at all, so the
  // Chat's guarded writes are no-ops here; the clear is guarded to match.)
  try {
    window.localStorage?.clear();
  } catch {
    /* localStorage unavailable in this environment */
  }
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      // `/api/portal/usage[...]` now carries a query string (page/limit/filters/
      // scope/range), so an exact `path === "/api/portal/usage"` compare no
      // longer matches. Switch the three usage endpoints on the parsed pathname.
      const usagePathname = (() => {
        try {
          return new URL(path, 'http://localhost').pathname;
        } catch {
          return path;
        }
      })();

      if (path === '/api/auth/login' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as { email: string; password: string };
        if (body.password !== 'dev-secret') {
          return jsonResponse(
            { error: { code: 'auth.invalid_credentials', message: 'invalid credentials' } },
            401,
          );
        }
        loggedIn = true;
        return jsonResponse(devUser);
      }
      if (path === '/api/auth/logout' && init?.method === 'POST') {
        loggedIn = false;
        return jsonResponse({ ok: true });
      }
      if (path === '/api/auth/session') {
        return jsonResponse(
          loggedIn
            ? {
                authenticated: true,
                user: {
                  ...devUser,
                  role: currentRole,
                  system_admin_mode: systemAdminModeMock,
                  system_admin_mode_expires_at: '',
                  system_admin_mode_require_password: true,
                },
                default_language: sessionLanguage,
              }
            : { authenticated: false, default_language: sessionLanguage },
        );
      }
      if (path === '/api/portal/me') {
        if (!loggedIn) {
          return jsonResponse(
            { error: { code: 'auth.session_invalid', message: 'session expired' } },
            401,
          );
        }
        return jsonResponse({
          ...devUser,
          role: currentRole,
          system_admin_mode: systemAdminModeMock,
          system_admin_mode_expires_at: '',
          system_admin_mode_require_password: true,
        });
      }
      // Step-up elevation: POST enters System-Admin mode, DELETE leaves. Both
      // flip the shared scope flag so a subsequent /me (or /groups) reflects the
      // new scope — mirroring the backend, whose session `elevated_until` drives
      // both the returned CurrentUser and every scope-gated list.
      if (path === '/api/portal/system-admin-mode' && init?.method === 'POST') {
        systemAdminModeMock = true;
        // One hour out: comfortably beyond the test yet under setTimeout's ~24.8d
        // clamp, so the App's auto-drop timer never fires during the test.
        return jsonResponse({
          ...devUser,
          role: currentRole,
          system_admin_mode: true,
          system_admin_mode_expires_at: new Date(Date.now() + 3_600_000).toISOString(),
          system_admin_mode_require_password: true,
        });
      }
      if (path === '/api/portal/system-admin-mode' && init?.method === 'DELETE') {
        systemAdminModeMock = false;
        return jsonResponse({
          ...devUser,
          role: currentRole,
          system_admin_mode: false,
          system_admin_mode_expires_at: '',
          system_admin_mode_require_password: true,
        });
      }
      // Per-user UI preferences (generic KV). The authenticated shell mounts a
      // PreferencesProvider that GETs these on mount and PUTs debounced writes;
      // answer both so no 404 noise (the provider swallows errors either way).
      if (path === '/api/portal/preferences' && (!init?.method || init.method === 'GET')) {
        return jsonResponse({});
      }
      if (path.startsWith('/api/portal/preferences/') && init?.method === 'PUT') {
        return jsonResponse({ ok: true });
      }
      if (path === '/api/portal/dashboard') {
        const totalTokens = apiState.usageRows.reduce((sum, row) => sum + row.total_tokens, 0);
        return jsonResponse({
          metrics: {
            requests_24h: apiState.usageRows.length,
            tokens_24h: totalTokens,
            healthy_hosts: '1/1',
            latency_p95_ms: 14,
          },
          routes: [
            {
              id: 'route_mock_qwen',
              model: 'qwen-coder',
              provider: 'mock',
              host: 'mock-host-qwen',
              status: 'active',
            },
          ],
        });
      }
      if (path === '/api/portal/tokens' && init?.method !== 'POST') {
        return jsonResponse({ data: apiState.tokenRows });
      }
      if (path === '/api/portal/tokens' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as { name: string; scopes: string[] };
        const token = {
          id: 'tok_created',
          name: body.name,
          secret_prefix: 'opaigw_',
          status: 'active',
          scopes: body.scopes,
          expires_at: null,
          last_used_at: null,
          created_at: '2026-07-10T12:02:00Z',
        };
        apiState.tokenRows = [token, ...apiState.tokenRows];
        return jsonResponse({ token, secret: 'opaigw_created_secret' }, 201);
      }
      if (usagePathname === '/api/portal/usage') {
        // Paged envelope (UsagePage). scope=all (system-admin) fills user_name.
        const search = new URL(path, 'http://localhost').searchParams;
        const page = Number(search.get('page')) || 1;
        const limit = Number(search.get('limit')) || 25;
        const scopeAll = search.get('scope') === 'all';
        const rows = apiState.usageRows.map((row) =>
          scopeAll ? { ...row, user_name: 'Dev User' } : { ...row },
        );
        const total = rows.length;
        return jsonResponse({
          data: rows,
          page,
          limit,
          total,
          total_pages: Math.ceil(total / limit),
        });
      }
      if (usagePathname === '/api/portal/usage/stats') {
        // Aggregate envelope (UsageStats). Totals over ALL rows; error predicate is
        // status==="error" || http_status>=400 (matches the list filter and chip).
        // The shell test only asserts the table, so empty histograms are enough —
        // SpeedHistogram bins are covered in Activity.test.tsx.
        const rows = apiState.usageRows;
        const isError = (r: (typeof rows)[number]) =>
          r.status === 'error' || (r.http_status ?? 0) >= 400;
        const emptyHistogram = { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 };
        return jsonResponse({
          totals: {
            total_requests: rows.length,
            error_count: rows.filter(isError).length,
            cached_tokens: rows.reduce((sum, r) => sum + (r.cached_tokens ?? 0), 0),
            input_tokens: rows.reduce((sum, r) => sum + r.input_tokens, 0),
            output_tokens: rows.reduce((sum, r) => sum + r.output_tokens, 0),
          },
          prompt_per_second: emptyHistogram,
          tokens_per_second: emptyHistogram,
        });
      }
      if (usagePathname === '/api/portal/usage/events') {
        // SSE is consumed by EventSource (stubbed above), not fetch. Answer
        // defensively with an immediately-closed event-stream so a stray fetch
        // never 404s and trips Activity's error Alert.
        const empty = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.close();
          },
        });
        return new Response(empty, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }
      // The admin management listing (?manage=1) additionally includes a hidden
      // model so ModelList/ModelGroupSection can revert/regroup it.
      if (path === '/api/portal/models?manage=1') {
        return jsonResponse({
          data: [
            {
              id: 'qwen-coder',
              display_name: 'qwen-coder',
              flavors: ['anthropic', 'openai'],
              visibility: 'shown',
            },
            { id: 'api-only', display_name: 'api-only', flavors: ['openai'], visibility: 'shown' },
            {
              id: 'anthropic-only',
              display_name: 'anthropic-only',
              flavors: ['anthropic'],
              visibility: 'shown',
            },
            {
              id: 'hidden-model',
              display_name: 'hidden-model',
              flavors: ['openai', 'anthropic'],
              visibility: 'hidden',
            },
          ],
        });
      }
      if (path === '/api/portal/models') {
        return jsonResponse({
          data: [
            { id: 'qwen-coder', display_name: 'qwen-coder', flavors: ['anthropic', 'openai'] },
            { id: 'api-only', display_name: 'api-only', flavors: ['openai'] },
            { id: 'anthropic-only', display_name: 'anthropic-only', flavors: ['anthropic'] },
          ],
        });
      }
      // Model groups (admin Models view renders ModelGroupSection, which loads these).
      if (path === '/api/portal/model-groups' && init?.method !== 'POST') {
        return jsonResponse({ data: [] });
      }
      // Per-model detail sub-view (ModelServersSection) — carries a ?name= query.
      if (usagePathname === '/api/portal/model-servers') {
        return jsonResponse({ data: [] });
      }
      // NetBird module-enabled flag: any portal user can read this (boolean-only,
      // no secret); App.tsx uses `module_enabled` (the raw enable checkbox) to
      // gate the NetBird nav item + view, and ServerList reads the SAME endpoint's
      // `enabled` (fully configured — checkbox + url + token) to gate its own
      // netbird row actions/columns — both driven by netbirdModuleEnabledMock
      // (default false) so only the NetBird-view nav tests below opt in;
      // netbirdRawCheckboxMock lets a test diverge `module_enabled` from
      // `enabled` (checkbox on, module not yet fully configured).
      if (path === '/api/portal/netbird/enabled') {
        return jsonResponse({
          enabled: netbirdModuleEnabledMock,
          module_enabled: netbirdRawCheckboxMock ?? netbirdModuleEnabledMock,
          netbird_only: false,
          manage_policies: false,
          effective_policy_scope: '',
          deny_by_default: false,
        });
      }
      if (path === '/v1/chat/completions' && init?.method === 'POST') {
        const body = JSON.parse(String(init?.body)) as { model: string };
        const enc = new TextEncoder();
        if (chatStreamMode === 'abort') {
          // Deliver one partial content chunk on the first read, then error the
          // stream with an AbortError on the next read to simulate the user
          // clicking Stop mid-stream. (A pull-based source is required: calling
          // controller.error() in start() would discard the queued chunk.)
          let pulls = 0;
          const aborted = new ReadableStream<Uint8Array>({
            pull(c) {
              if (pulls === 0) {
                c.enqueue(enc.encode(`data: {"choices":[{"delta":{"content":"partial"}}]}\n\n`));
                pulls += 1;
              } else {
                c.error(new DOMException('aborted', 'AbortError'));
              }
            },
          });
          return new Response(aborted, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }
        if (chatStreamMode === 'pending') {
          // Deliver one partial content chunk, then leave the stream open
          // forever (the second pull returns a never-resolving promise so the
          // reader's next read stays pending without a busy loop). This models a
          // long-running background stream that must survive a view change.
          let pulls = 0;
          const pending = new ReadableStream<Uint8Array>({
            pull(c) {
              if (pulls === 0) {
                c.enqueue(
                  enc.encode(`data: {"choices":[{"delta":{"content":"partial-pending"}}]}\n\n`),
                );
                pulls += 1;
                return;
              }
              return new Promise<void>(() => {});
            },
          });
          return new Response(pending, {
            status: 200,
            headers: { 'Content-Type': 'text/event-stream' },
          });
        }
        let lines: string[];
        if (chatStreamMode === 'reasoning') {
          lines = [
            `data: {"choices":[{"delta":{"reasoning_content":"Because reasons"}}]}\n\n`,
            `data: {"choices":[{"delta":{"content":"Answer for ${body.model}"}}]}\n\n`,
            `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n`,
            `data: [DONE]\n\n`,
          ];
        } else if (chatStreamMode === 'error') {
          lines = [`data: {"error":{"code":"provider.unavailable","message":"boom"}}\n\n`];
        } else {
          lines = [
            `data: {"choices":[{"delta":{"content":"Mock stream for ${body.model}"}}]}\n\n`,
            `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n`,
            `data: [DONE]\n\n`,
          ];
        }
        const stream = new ReadableStream<Uint8Array>({
          start(c) {
            for (const l of lines) c.enqueue(enc.encode(l));
            c.close();
          },
        });
        return new Response(stream, {
          status: 200,
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }
      if (path === '/api/portal/servers' && (!init?.method || init.method === 'GET')) {
        return jsonResponse({ data: apiState.serverRows });
      }
      if (path === '/api/portal/servers' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as {
          name: string;
          domain: string;
          server_path_suffix?: string;
          status?: ServerStatus;
          owner_ids?: string[];
          netbird_enabled?: boolean;
          admin_group_ids?: string[];
        };
        const owners = (body.owner_ids ?? [])
          .map((id) => apiState.userRows.find((user) => user.id === id))
          .filter((user): user is (typeof apiState.userRows)[number] => Boolean(user))
          .map((user) => ({ id: user.id, email: user.email, display_name: user.display_name }));
        const adminGroups = (body.admin_group_ids ?? [])
          .map((gid) => defaultAdminGroupCandidates.find((c) => c.id === gid))
          .filter((c): c is (typeof defaultAdminGroupCandidates)[number] => Boolean(c))
          .map((c) => ({ id: c.id, name: c.name }));
        const server: PortalServer = {
          id: 'srv_created',
          name: body.name,
          domain: body.domain,
          server_path_suffix: body.server_path_suffix ?? '',
          status: body.status ?? 'active',
          health_status: 'unknown',
          owners,
          last_seen_at: null,
          created_at: '2026-07-11T09:00:00Z',
          netbird_enabled: Boolean(body.netbird_enabled),
          netbird_setup_key_id: '',
          netbird_group_id: '',
          netbird_peer_id: '',
          netbird_connected: false,
          netbird_group_ids: [],
          netbird_peer_managed: false,
          netbird_policy_override: '',
          netbird_allow_ping: false,
          netbird_ping_exclude: false,
          agent_status: 'unconfigured',
          agent_presence_timeout_seconds: 0,
          estimated_watts: 0,
          idle_watts: 0,
          price_per_kwh: 0,
          pue: 0,
          price_unit: 'eur_cent',
          admin_groups: adminGroups,
          system_group_id: adminGroups.length > 0 ? 'sg_default' : '',
          system_group_name: adminGroups.length > 0 ? 'Default System Group' : '',
        };
        apiState.serverRows = [server, ...apiState.serverRows];
        return jsonResponse(server, 201);
      }
      if (
        path === '/api/portal/server-admin-group-candidates' &&
        (!init?.method || init.method === 'GET')
      ) {
        return jsonResponse({ data: defaultAdminGroupCandidates });
      }
      if (
        path.startsWith('/api/portal/servers/') &&
        path.endsWith('/admin-groups') &&
        init?.method === 'PUT'
      ) {
        const id = path.substring(
          '/api/portal/servers/'.length,
          path.length - '/admin-groups'.length,
        );
        const body = JSON.parse(String(init.body)) as { admin_group_ids?: string[] };
        const adminGroups = (body.admin_group_ids ?? [])
          .map((gid) => defaultAdminGroupCandidates.find((c) => c.id === gid))
          .filter((c): c is (typeof defaultAdminGroupCandidates)[number] => Boolean(c))
          .map((c) => ({ id: c.id, name: c.name }));
        apiState.serverRows = apiState.serverRows.map((row) =>
          row.id === id
            ? {
                ...row,
                admin_groups: adminGroups,
                system_group_id: adminGroups.length > 0 ? 'sg_default' : row.system_group_id,
              }
            : row,
        );
        const updated = apiState.serverRows.find((row) => row.id === id);
        return jsonResponse(updated);
      }
      if (path.startsWith('/api/portal/servers/') && path.endsWith('/agent-token')) {
        const serverId = path.substring(
          '/api/portal/servers/'.length,
          path.length - '/agent-token'.length,
        );
        if (init?.method === 'POST') {
          apiState.agentTokenExists[serverId] = true;
          return jsonResponse(
            { secret: 'opaigw_agent_secret', token: { exists: true, secret_prefix: 'opaigw_' } },
            201,
          );
        }
        if (init?.method === 'DELETE') {
          apiState.agentTokenExists[serverId] = false;
          return jsonResponse({ ok: true });
        }
        const flag = apiState.agentTokenExists[serverId] ?? false;
        return jsonResponse({ exists: flag, secret_prefix: flag ? 'opaigw_' : undefined });
      }
      if (path.startsWith('/api/portal/servers/') && init?.method === 'PATCH') {
        const id = path.substring('/api/portal/servers/'.length);
        const body = JSON.parse(String(init.body)) as {
          name?: string;
          domain?: string;
          status?: ServerStatus;
          owner_ids?: string[];
        };
        apiState.serverRows = apiState.serverRows.map((row) => {
          if (row.id !== id) {
            return row;
          }
          const owners = body.owner_ids
            ? body.owner_ids
                .map((ownerId) => apiState.userRows.find((user) => user.id === ownerId))
                .filter((user): user is (typeof apiState.userRows)[number] => Boolean(user))
                .map((user) => ({
                  id: user.id,
                  email: user.email,
                  display_name: user.display_name,
                }))
            : row.owners;
          return {
            ...row,
            name: body.name ?? row.name,
            domain: body.domain ?? row.domain,
            status: body.status ?? row.status,
            owners,
          };
        });
        const updated = apiState.serverRows.find((row) => row.id === id);
        return jsonResponse(updated);
      }
      if (path.startsWith('/api/portal/servers/') && init?.method === 'DELETE') {
        const id = path.substring('/api/portal/servers/'.length);
        apiState.serverRows = apiState.serverRows.filter((row) => row.id !== id);
        return jsonResponse({ ok: true });
      }
      if (
        path.startsWith('/api/portal/servers/') &&
        path.endsWith('/applications') &&
        (!init?.method || init.method === 'GET')
      ) {
        const serverId = path.substring(
          '/api/portal/servers/'.length,
          path.length - '/applications'.length,
        );
        return jsonResponse({
          data: apiState.applicationRows.filter((row) => row.server_id === serverId),
        });
      }
      if (
        path.startsWith('/api/portal/servers/') &&
        path.endsWith('/applications') &&
        init?.method === 'POST'
      ) {
        const serverId = path.substring(
          '/api/portal/servers/'.length,
          path.length - '/applications'.length,
        );
        const server = apiState.serverRows.find((row) => row.id === serverId);
        const body = JSON.parse(String(init.body)) as {
          type: string;
          port: number;
          scheme: string;
          api_flavors?: string[];
          priority?: number;
          weight?: number;
          timeout_ms?: number;
          affinity_ttl_seconds?: number;
          admission_queue_timeout_seconds?: number;
          status?: string;
          always_reachable?: boolean;
          health_check_path?: string;
          health_check_mode?: PortalApplication['health_check_mode'];
          health_check_interval_seconds?: number;
          native_responses?: boolean;
          native_messages?: boolean;
          loaded_models_path?: string;
          loaded_models_format?: string;
          context_probe_path?: string;
          app_path_suffix?: string;
          api_token?: string;
          api_token_header?: string;
          benchmark_schedule_enabled?: boolean;
          benchmark_schedule_interval_seconds?: number;
          opportunistic_metrics_enabled?: boolean;
          proxy_listen_port?: number;
          proxy_excluded?: boolean;
        };
        const application: PortalApplication = {
          id: `app_${apiState.applicationRows.length + 1}`,
          server_id: serverId,
          type: body.type as PortalApplication['type'],
          port: body.port,
          scheme: body.scheme as PortalApplication['scheme'],
          endpoint: `${body.scheme}://${server?.domain ?? 'unknown'}:${body.port}`,
          api_flavors: body.api_flavors ?? ['openai', 'anthropic'],
          priority: body.priority ?? 0,
          weight: body.weight ?? 0,
          timeout_ms: body.timeout_ms ?? 30000,
          affinity_ttl_seconds: body.affinity_ttl_seconds ?? 1800,
          admission_queue_timeout_seconds: body.admission_queue_timeout_seconds ?? 0,
          status: (body.status as PortalApplication['status']) ?? 'active',
          always_reachable: body.always_reachable ?? false,
          health_check_path: body.health_check_path ?? '/v1/health',
          health_check_mode: body.health_check_mode ?? 'health_path',
          health_check_interval_seconds: body.health_check_interval_seconds ?? 0,
          native_responses: body.native_responses ?? false,
          native_messages: body.native_messages ?? false,
          loaded_models_path: body.loaded_models_path ?? '',
          loaded_models_format: body.loaded_models_format ?? '',
          context_probe_path: body.context_probe_path ?? '',
          app_path_suffix: body.app_path_suffix ?? '',
          api_token_set: Boolean(body.api_token),
          api_token_header: body.api_token_header ?? '',
          benchmark_schedule_enabled: body.benchmark_schedule_enabled ?? false,
          benchmark_schedule_interval_seconds: body.benchmark_schedule_interval_seconds ?? 0,
          opportunistic_metrics_enabled: body.opportunistic_metrics_enabled ?? false,
          proxy_listen_port: body.proxy_listen_port ?? 0,
          proxy_excluded: body.proxy_excluded ?? false,
          reachable: true,
          last_checked_at: null,
          created_at: '2026-07-11T10:00:00Z',
        };
        apiState.applicationRows = [application, ...apiState.applicationRows];
        return jsonResponse(application, 201);
      }
      if (
        path.startsWith('/api/portal/applications/') &&
        path.endsWith('/mappings') &&
        (!init?.method || init.method === 'GET')
      ) {
        const appId = path.substring(
          '/api/portal/applications/'.length,
          path.length - '/mappings'.length,
        );
        return jsonResponse({
          data: apiState.mappingRows.filter((row) => row.application_id === appId),
        });
      }
      if (
        path.startsWith('/api/portal/applications/') &&
        path.endsWith('/mappings') &&
        init?.method === 'POST'
      ) {
        const appId = path.substring(
          '/api/portal/applications/'.length,
          path.length - '/mappings'.length,
        );
        const body = JSON.parse(String(init.body)) as {
          gateway_model_name: string;
          app_model_name: string;
          status?: string;
        };
        const mapping: PortalModelMapping = {
          id: `map_${apiState.mappingRows.length + 1}`,
          application_id: appId,
          gateway_model_name: body.gateway_model_name,
          app_model_name: body.app_model_name,
          status: (body.status as PortalModelMapping['status']) ?? 'active',
          created_at: '2026-07-11T10:05:00Z',
          gen_tokens_per_second: 0,
          prompt_tokens_per_second: 0,
          load_time_ms: 0,
          context_size: 0,
          is_mtp: false,
          vision_capable: false,
          energy_wh_per_token: 0,
          metrics_locked: false,
          metrics_source: '',
          metrics_updated_at: null,
          max_concurrency: 0,
          recommended_concurrency: 0,
          gen_tokens_per_second_at_capacity: 0,
        };
        apiState.mappingRows = [mapping, ...apiState.mappingRows];
        return jsonResponse(mapping, 201);
      }
      if (
        path.startsWith('/api/portal/applications/') &&
        path.endsWith('/sync-models') &&
        init?.method === 'POST'
      ) {
        const appId = path.substring(
          '/api/portal/applications/'.length,
          path.length - '/sync-models'.length,
        );
        const synced: PortalModelMapping = {
          id: `map_synced_${apiState.mappingRows.length + 1}`,
          application_id: appId,
          gateway_model_name: 'synced-model',
          app_model_name: 'synced-model',
          status: 'active',
          created_at: '2026-07-11T10:06:00Z',
          gen_tokens_per_second: 0,
          prompt_tokens_per_second: 0,
          load_time_ms: 0,
          context_size: 0,
          is_mtp: false,
          vision_capable: false,
          energy_wh_per_token: 0,
          metrics_locked: false,
          metrics_source: '',
          metrics_updated_at: null,
          max_concurrency: 0,
          recommended_concurrency: 0,
          gen_tokens_per_second_at_capacity: 0,
        };
        apiState.mappingRows = [synced, ...apiState.mappingRows];
        return jsonResponse({ added: 1, disabled: 0, unchanged: 0, conflicted: 0 });
      }
      if (path.startsWith('/api/portal/applications/') && init?.method === 'PATCH') {
        const id = path.substring('/api/portal/applications/'.length);
        const body = JSON.parse(String(init.body)) as Partial<PortalApplication>;
        apiState.applicationRows = apiState.applicationRows.map((row) =>
          row.id === id ? { ...row, ...body } : row,
        );
        const updated = apiState.applicationRows.find((row) => row.id === id);
        return jsonResponse(updated);
      }
      if (path.startsWith('/api/portal/applications/') && init?.method === 'DELETE') {
        const id = path.substring('/api/portal/applications/'.length);
        apiState.applicationRows = apiState.applicationRows.filter((row) => row.id !== id);
        apiState.mappingRows = apiState.mappingRows.filter((row) => row.application_id !== id);
        return jsonResponse({ ok: true });
      }
      if (
        path.startsWith('/api/portal/applications/') &&
        (!init?.method || init.method === 'GET')
      ) {
        const id = path.substring('/api/portal/applications/'.length);
        const found = apiState.applicationRows.find((row) => row.id === id);
        if (!found) {
          return jsonResponse(
            { error: { code: 'application.not_found', message: 'not found' } },
            404,
          );
        }
        return jsonResponse(found);
      }
      if (path.startsWith('/api/portal/mappings/') && init?.method === 'PATCH') {
        const id = path.substring('/api/portal/mappings/'.length);
        const body = JSON.parse(String(init.body)) as Partial<PortalModelMapping>;
        apiState.mappingRows = apiState.mappingRows.map((row) =>
          row.id === id ? { ...row, ...body } : row,
        );
        const updated = apiState.mappingRows.find((row) => row.id === id);
        return jsonResponse(updated);
      }
      if (path.startsWith('/api/portal/mappings/') && init?.method === 'DELETE') {
        const id = path.substring('/api/portal/mappings/'.length);
        apiState.mappingRows = apiState.mappingRows.filter((row) => row.id !== id);
        return jsonResponse({ ok: true });
      }
      if (path === '/api/admin/users' && (!init?.method || init.method === 'GET')) {
        return jsonResponse({ data: apiState.userRows });
      }
      // The invite-form admin-group picker (UsersView) fetches the group
      // landscape on open; a single manageable admin group auto-selects (no
      // control shown), keeping the pre-existing invite-flow test unblocked.
      if (path === '/api/portal/groups' && (!init?.method || init.method === 'GET')) {
        return jsonResponse({
          // System-tier groups are only in the caller's scope while elevated —
          // the backend withholds the `system` scope until step-up. A stale
          // pre-elevation fetch returns [] here; the view must re-fetch on
          // elevation to populate the System panel.
          system: systemAdminModeMock
            ? [
                {
                  id: 'grp-global-system',
                  tier: 'system',
                  name: 'Global System Group',
                  parent_group_id: '',
                  owner_user_id: '',
                  owner_name: '',
                  my_role: 'member',
                  can_manage: true,
                  member_count: 1,
                  manager_count: 0,
                },
              ]
            : [],
          admin: [
            {
              id: 'grp-default-admin',
              tier: 'admin',
              name: 'Default Admin Group',
              parent_group_id: '',
              owner_user_id: 'usr-owner',
              my_role: 'member',
              can_manage: true,
              // can_manage_users (spec 2026-08-10): the invite-picker filter
              // source -- must be true for the pre-existing single-manageable-
              // admin-group auto-select invite-flow test to keep passing.
              can_manage_users: true,
              member_count: 1,
              manager_count: 0,
            },
          ],
          user: [],
        });
      }
      if (path === '/api/admin/users' && init?.method === 'POST') {
        const user = {
          id: 'usr_new',
          email: 'new@example.test',
          display_name: 'New',
          role: 'user',
          status: 'invited',
          preferred_language: 'de',
          created_at: '2026-07-11T09:00:00Z',
        };
        apiState.userRows = [...apiState.userRows, user];
        return jsonResponse(
          { user, invite_url: 'http://localhost:8080/set-password?token=abc' },
          201,
        );
      }
      if (path === '/api/portal/password' && init?.method === 'POST') {
        return jsonResponse({ ok: true });
      }
      if (path.startsWith('/api/portal/tokens/') && init?.method === 'PATCH') {
        const id = path.substring('/api/portal/tokens/'.length);
        const body = JSON.parse(String(init.body)) as {
          name?: string;
          scopes?: string[];
          status?: string;
        };
        apiState.tokenRows = apiState.tokenRows.map((row) =>
          row.id === id
            ? {
                ...row,
                name: body.name ?? row.name,
                scopes: body.scopes ?? row.scopes,
                status: body.status ?? row.status,
              }
            : row,
        );
        const updated = apiState.tokenRows.find((row) => row.id === id);
        return jsonResponse(updated);
      }
      if (path.startsWith('/api/portal/tokens/') && init?.method === 'DELETE') {
        const id = path.substring('/api/portal/tokens/'.length);
        apiState.tokenRows = apiState.tokenRows.filter((row) => row.id !== id);
        return jsonResponse({ ok: true });
      }
      if (path === '/api/system/theme') {
        // ThemeRoot fetches this on mount (pre-auth) and again after a theme save.
        return jsonResponse({ theme: 'default' });
      }
      if (path === '/api/system/settings' && (!init?.method || init.method === 'GET')) {
        return jsonResponse({
          theme: 'default',
          available_themes: [{ id: 'default', name: 'Default' }],
          language: 'de',
          available_languages: ['de', 'en'],
          capture_retention_days: 30,
          capture_enabled: true,
          capture_override: false,
          health_check_interval_seconds: 30,
        });
      }
      if (path === '/api/system/settings' && init?.method === 'PUT') {
        const body = JSON.parse(String(init.body)) as {
          theme?: string;
          language?: string;
          capture_retention_days?: number;
          capture_enabled?: boolean;
          health_check_interval_seconds?: number;
          netbird_enabled?: boolean;
        };
        // Reflect a saved NetBird enable toggle so the App's onSaved re-fetch of
        // /api/portal/netbird/enabled returns the new module_enabled (live nav update).
        if (body.netbird_enabled !== undefined) netbirdRawCheckboxMock = body.netbird_enabled;
        return jsonResponse({
          theme: body.theme ?? 'default',
          available_themes: [{ id: 'default', name: 'Default' }],
          language: body.language ?? 'de',
          available_languages: ['de', 'en'],
          capture_retention_days: body.capture_retention_days ?? 30,
          capture_enabled: body.capture_enabled ?? true,
          health_check_interval_seconds: body.health_check_interval_seconds ?? 30,
          netbird_enabled: body.netbird_enabled ?? false,
        });
      }
      if (path === '/api/portal/language' && init?.method === 'PUT') {
        const body = init?.body ? JSON.parse(init.body as string) : {};
        return jsonResponse({
          ...devUser,
          role: currentRole,
          preferred_language: body.language,
          system_admin_mode: systemAdminModeMock,
          system_admin_mode_expires_at: '',
          system_admin_mode_require_password: true,
        });
      }
      // Persistent chats: the ChatStoreProvider (mounted above the view switch)
      // loads the list on auth and opens/creates a chat, so these must be served
      // in every shell test, not only the chat-view ones. Stateful in-memory
      // store; summaries omit `content`.
      // Background chat runs: the store starts a server run (POST .../runs),
      // subscribes to it via EventSource (handled by MockEventSource below), and
      // may cancel it (POST .../runs/{id}/cancel) or list active runs. These
      // must be matched BEFORE the generic `/api/portal/chats/{id}` handlers so
      // the run sub-paths are not swallowed.
      if (
        usagePathname === '/api/portal/chats/runs/active' &&
        (!init?.method || init.method === 'GET')
      ) {
        return jsonResponse({ data: [] });
      }
      {
        const runStart = usagePathname.match(/^\/api\/portal\/chats\/([^/]+)\/runs$/);
        if (runStart && init?.method === 'POST') {
          return jsonResponse({
            run_id: `run_${runStart[1]}`,
            chat_id: runStart[1],
            status: 'running',
          });
        }
        const runCancel = usagePathname.match(
          /^\/api\/portal\/chats\/([^/]+)\/runs\/([^/]+)\/cancel$/,
        );
        if (runCancel && init?.method === 'POST') {
          return jsonResponse({ ok: true });
        }
      }
      if (path === '/api/portal/chats' && (!init?.method || init.method === 'GET')) {
        const summaries = apiState.chatRows.map(({ content: _content, ...rest }) => rest);
        return jsonResponse({ data: summaries });
      }
      if (path === '/api/portal/chats' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as { title?: string; content?: unknown };
        const now = '2026-07-17T12:00:00Z';
        const chat = {
          id: `chat_${apiState.chatRows.length + 1}`,
          title: body.title ?? '',
          created_at: now,
          updated_at: now,
          content: body.content ?? { settings: {}, messages: [] },
        };
        apiState.chatRows = [chat, ...apiState.chatRows];
        return jsonResponse(chat, 201);
      }
      if (path.startsWith('/api/portal/chats/') && (!init?.method || init.method === 'GET')) {
        const id = path.substring('/api/portal/chats/'.length);
        const chat = apiState.chatRows.find((row) => row.id === id);
        if (!chat)
          return jsonResponse({ error: { code: 'chat.not_found', message: 'not found' } }, 404);
        return jsonResponse(chat);
      }
      if (path.startsWith('/api/portal/chats/') && init?.method === 'PUT') {
        const id = path.substring('/api/portal/chats/'.length);
        const body = JSON.parse(String(init.body)) as { title: string; content: unknown };
        apiState.chatRows = apiState.chatRows.map((row) =>
          row.id === id
            ? {
                ...row,
                title: body.title,
                content: body.content,
                updated_at: '2026-07-17T12:05:00Z',
              }
            : row,
        );
        const updated = apiState.chatRows.find((row) => row.id === id);
        return jsonResponse(updated);
      }
      if (path.startsWith('/api/portal/chats/') && init?.method === 'DELETE') {
        const id = path.substring('/api/portal/chats/'.length);
        apiState.chatRows = apiState.chatRows.filter((row) => row.id !== id);
        return jsonResponse({ ok: true });
      }
      return jsonResponse({ error: { code: 'request.not_found', message: 'not found' } }, 404);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  // ThemeRoot writes the resolved mode/vars onto the shared documentElement;
  // clear them so a test's initial-mode assertion can't see a prior render's state.
  document.documentElement.removeAttribute('style');
  delete document.documentElement.dataset.mode;
});

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('App', () => {
  // Nav ist persistent (Sidebar) — Link direkt klicken (sobald authentifiziert).
  const gotoNav = async (name: string | RegExp) => {
    fireEvent.click(await screen.findByRole('link', { name }));
  };

  // The chat no longer auto-selects a model, so tests that send must pick one
  // first (opens the model dropdown and clicks the option by name).
  const pickChatModel = async (name = 'qwen-coder') => {
    fireEvent.mouseDown(await screen.findByRole('combobox', { name: messages.de.chatModel }));
    fireEvent.click(await screen.findByRole('option', { name }));
  };

  it('renders the OP AI Gateway shell in German', async () => {
    renderApp();

    expect(await screen.findByText('Dev User')).toBeInTheDocument();
    expect(screen.getByText('On-Prem')).toBeInTheDocument();
    expect(screen.getByText('AI Gateway')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Chat' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'API-Tokens' })).toBeInTheDocument();
    expect(screen.queryByText(/Lorenz/)).not.toBeInTheDocument();
  });

  it('switches the applied color mode from the menu', async () => {
    renderApp();
    await screen.findByText('Dev User');
    await waitFor(() => expect(document.documentElement.dataset.mode).toBe('light'));

    fireEvent.click(screen.getByRole('button', { name: messages.de.colorMode }));
    fireEvent.click(await screen.findByRole('menuitem', { name: messages.de.colorModeDark }));
    await waitFor(() => expect(document.documentElement.dataset.mode).toBe('dark'));

    fireEvent.click(screen.getByRole('button', { name: messages.de.colorMode }));
    fireEvent.click(await screen.findByRole('menuitem', { name: messages.de.colorModeLight }));
    await waitFor(() => expect(document.documentElement.dataset.mode).toBe('light'));
  });

  it('renders all primary navigation items and marks dashboard active', async () => {
    renderApp();
    await screen.findByText('Dev User');
    for (const label of [
      'Dashboard',
      'Chat',
      'API-Tokens',
      'Aktivität',
      'Modelle',
      'AI Server',
      'Benutzer',
      'Tools',
    ]) {
      expect(screen.getByRole('link', { name: label })).toBeInTheDocument();
    }
    expect(screen.queryByRole('link', { name: 'Policies' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Management' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Dashboard' })).toHaveAttribute('aria-current', 'page');
  });

  it('toggles the navigation sidebar from the topbar', async () => {
    renderApp();
    await screen.findByText('Dev User');
    // Startet expandiert (Test-Viewport ist breit) -> Button bietet Einklappen an.
    const collapse = screen.getByRole('button', { name: messages.de.collapseNavigation });
    fireEvent.click(collapse);
    expect(screen.getByRole('button', { name: messages.de.openNavigation })).toBeInTheDocument();
  });

  it('loads dashboard data from portal APIs and renders footer labels', async () => {
    renderApp();

    expect(await screen.findByText('1')).toBeInTheDocument();
    expect(screen.getByText('8')).toBeInTheDocument();
    expect(screen.getByText('1/1')).toBeInTheDocument();
    expect(screen.getByText('14 ms')).toBeInTheDocument();
    expect(screen.getByText('Requests 24h')).toBeInTheDocument();
    expect(screen.getByText('Tokens 24h')).toBeInTheDocument();
    expect(screen.getByText('Server gesund')).toBeInTheDocument();
    expect(screen.getByText('Latenz p95')).toBeInTheDocument();
    expect(
      screen.getByText('Copyright (C) 2026 OnPrem AI Gateway contributors'),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'AGPL-3.0' })).toHaveAttribute(
      'href',
      'https://www.gnu.org/licenses/agpl-3.0.html',
    );
    expect(screen.getByRole('link', { name: 'Quelltext' })).toHaveAttribute(
      'href',
      'https://github.com/JLor08/op-ai-gateway',
    );
    expect(screen.getByRole('button', { name: 'Datenschutz' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Nutzungsbedingungen' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Impressum' })).toBeInTheDocument();
  });

  it('navigates to the Impressum template from the footer', async () => {
    renderApp();
    await screen.findByText('Dev User');
    fireEvent.click(screen.getByRole('button', { name: 'Impressum' }));
    expect(await screen.findByText(/Vorlage/i)).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Impressum' })).toBeInTheDocument();
  });

  it('renders API route table headers and route rows', async () => {
    renderApp();

    const table = await screen.findByRole('table');
    expect(within(table).getByRole('columnheader', { name: 'Modell' })).toBeInTheDocument();
    expect(within(table).getByRole('columnheader', { name: 'Provider' })).toBeInTheDocument();
    expect(within(table).getByRole('columnheader', { name: 'Server' })).toBeInTheDocument();
    expect(within(table).getByRole('columnheader', { name: 'Status' })).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: 'qwen-coder' })).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: 'mock-host-qwen' })).toBeInTheDocument();
    expect(within(table).getByText('Aktiv').closest('[data-status]')).toHaveAttribute(
      'data-status',
      'active',
    );
  });

  it('toggles the shell language between German and English', async () => {
    renderApp();

    await screen.findByText('Dev User');

    fireEvent.click(screen.getByRole('button', { name: messages.de.language }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'EN' }));

    expect(screen.getByText('Healthy servers')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Privacy' })).toBeInTheDocument();

    await gotoNav('Chat');
    expect(screen.getByLabelText('Message')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: messages.en.language }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'DE' }));
    expect(await screen.findByLabelText('Nachricht')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Senden' })).toBeInTheDocument();
  });

  it('opens chat and streams a chat completion reply', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Hallo Gateway' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    // "Hallo Gateway" also becomes the chat's auto-title in the sidebar, so scope
    // the transcript assertion to the message log.
    expect(await screen.findByText('Mock stream for qwen-coder')).toBeInTheDocument();
    expect(within(screen.getByRole('log')).getByText('Hallo Gateway')).toBeInTheDocument();
  });

  it('uses live-region chat semantics and replaces the empty state after sending', async () => {
    renderApp();

    await gotoNav('Chat');

    const chatLog = screen.getByRole('log', { name: 'Chatverlauf' });
    expect(chatLog).toHaveAttribute('aria-live', 'polite');
    expect(chatLog).toHaveAttribute('aria-relevant', 'additions text');
    expect(within(chatLog).getByText('Noch keine Nachrichten.')).toBeInTheDocument();

    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Hallo Gateway' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    expect(await within(chatLog).findByText('Mock stream for qwen-coder')).toBeInTheDocument();
    expect(within(chatLog).queryByText('Noch keine Nachrichten.')).not.toBeInTheDocument();
  });

  it('does not send blank chat messages', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: '   ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    // A blank message must not start a run (the run-subscriber path POSTs to
    // /api/portal/chats/{id}/runs, never /v1/chat/completions).
    expect(
      (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([path, init]) =>
          /\/api\/portal\/chats\/[^/]+\/runs$/.test(String(path)) &&
          (init as RequestInit | undefined)?.method === 'POST',
      ),
    ).toHaveLength(0);
  });

  it('renders a reasoning delta in the reasoning block', async () => {
    chatStreamMode = 'reasoning';
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Denk nach' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    const reasoning = await screen.findByText('Because reasons');
    expect(reasoning.closest('details')).not.toBeNull();
    expect(await screen.findByText('Answer for qwen-coder')).toBeInTheDocument();
  });

  it('does not show an error banner when the stream is aborted', async () => {
    chatStreamMode = 'abort';
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Stop me' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    // the partial content delivered before the abort remains visible
    expect(await screen.findByText('partial')).toBeInTheDocument();
    // no error toast is raised for the AbortError
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  });

  it('clears the transcript with New Chat', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Hallo Gateway' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));
    await screen.findByText('Mock stream for qwen-coder');

    fireEvent.click(screen.getByRole('button', { name: 'Neuer Chat' }));

    // New Chat now creates + activates a fresh server chat asynchronously, so the
    // empty state appears once the new chat is loaded.
    expect(await screen.findByText('Noch keine Nachrichten.')).toBeInTheDocument();
    expect(
      within(screen.getByRole('log')).queryByText('Mock stream for qwen-coder'),
    ).not.toBeInTheDocument();
  });

  it('shows an error banner when the stream emits an error frame', async () => {
    chatStreamMode = 'error';
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Kaputt' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent('boom');

    // the pre-appended empty assistant placeholder must be pruned, leaving no
    // empty bubble and no stray Copy button
    await waitFor(() => expect(document.querySelector('[data-role="assistant"]')).toBeNull());
    expect(screen.queryByRole('button', { name: messages.de.chatCopy })).not.toBeInTheDocument();
  });

  it('keeps a background chat stream running across a view change (no abort on nav)', async () => {
    chatStreamMode = 'pending';
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    await pickChatModel();
    fireEvent.change(screen.getByLabelText('Nachricht'), { target: { value: 'Lauf weiter' } });
    fireEvent.click(screen.getByRole('button', { name: 'Senden' }));

    // The partial content arrives while the stream is still open.
    expect(await screen.findByText('partial-pending')).toBeInTheDocument();

    // Leave the chat view: the ChatStoreProvider sits above the view switch, so
    // the stream is NOT aborted — the nav still reports a running background
    // stream even though the Chat view is unmounted.
    await gotoNav('Dashboard');
    expect(await screen.findByTestId('chat-streaming')).toBeInTheDocument();
    expect(screen.queryByLabelText('Nachricht')).not.toBeInTheDocument();

    // Returning shows the partial reply intact (transcript preserved in memory,
    // stream never aborted).
    await gotoNav(/Chat/i);
    expect(await screen.findByText('partial-pending')).toBeInTheDocument();
    expect(screen.getByTestId('chat-streaming')).toBeInTheDocument();
  });

  it('rejects an unsupported attachment and shows the image error', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');

    const fileInput = screen.getByLabelText(messages.de.chatAttachImage);
    const badFile = new File(['x'], 'notes.txt', { type: 'text/plain' });
    fireEvent.change(fileInput, { target: { files: [badFile] } });

    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(messages.de.chatImageErrorType);
    // no misleading "Portal API Fehler:" prefix on an attach error
    expect(banner).not.toHaveTextContent(messages.de.portalError);
    expect(screen.queryByAltText(messages.de.chatAttachedImage)).not.toBeInTheDocument();
  });

  it('rejects an oversized image attachment and shows the size error', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');

    const fileInput = screen.getByLabelText(messages.de.chatAttachImage);
    const bigFile = new File(['x'], 'huge.jpg', { type: 'image/jpeg' });
    Object.defineProperty(bigFile, 'size', { value: 21 * 1024 * 1024 });
    fireEvent.change(fileInput, { target: { files: [bigFile] } });

    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(messages.de.chatImageErrorSize);
    expect(banner).not.toHaveTextContent(messages.de.portalError);
    expect(screen.queryByAltText(messages.de.chatAttachedImage)).not.toBeInTheDocument();
  });

  it('attaches a single valid image with no error banner', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');

    const fileInput = screen.getByLabelText(messages.de.chatAttachImage);
    const goodFile = new File(['x'], 'a.jpg', { type: 'image/jpeg' });
    fireEvent.change(fileInput, { target: { files: [goodFile] } });

    await screen.findByAltText(messages.de.chatAttachedImage);
    expect(screen.queryAllByAltText(messages.de.chatAttachedImage)).toHaveLength(1);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('caps a six-image batch at five and shows the count error', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');

    const fileInput = screen.getByLabelText(messages.de.chatAttachImage);
    const files = Array.from(
      { length: 6 },
      (_, i) => new File(['x'], `img-${i}.jpg`, { type: 'image/jpeg' }),
    );
    fireEvent.change(fileInput, { target: { files } });

    await waitFor(() =>
      expect(screen.queryAllByAltText(messages.de.chatAttachedImage)).toHaveLength(5),
    );
    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(messages.de.chatImageErrorCount);
    expect(banner).not.toHaveTextContent(messages.de.portalError);
  });

  it('keeps the valid image from a mixed batch and reports the bad one', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');

    const fileInput = screen.getByLabelText(messages.de.chatAttachImage);
    const badFile = new File(['x'], 'notes.txt', { type: 'text/plain' });
    const goodFile = new File(['x'], 'a.jpg', { type: 'image/jpeg' });
    fireEvent.change(fileInput, { target: { files: [badFile, goodFile] } });

    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(messages.de.chatImageErrorType);
    expect(banner).not.toHaveTextContent(messages.de.portalError);
    await waitFor(() =>
      expect(screen.queryAllByAltText(messages.de.chatAttachedImage)).toHaveLength(1),
    );
  });

  it('shows API tokens with scopes, active status, and row actions', async () => {
    renderApp();

    await gotoNav(/API-Tokens/i);

    expect(screen.getByRole('heading', { name: 'API-Tokens' })).toBeInTheDocument();
    expect(await screen.findByText('Dev Token')).toBeInTheDocument();
    expect(screen.getByText('gateway:use, admin')).toBeInTheDocument();
    expect(screen.getByText('Aktiv').closest('[data-status]')).toHaveAttribute(
      'data-status',
      'active',
    );
    expect(screen.getByRole('button', { name: 'Token erstellen' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Bearbeiten' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Deaktivieren' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Löschen' }).length).toBeGreaterThan(0);
  });

  it('creates a token through the portal API and displays the one-time secret', async () => {
    renderApp();

    await gotoNav(/API-Tokens/i);
    await screen.findByText('Dev Token');
    // The create form is now a sub-view mask: open it, fill the name, submit.
    fireEvent.click(screen.getByRole('button', { name: 'Token erstellen' }));
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'My CLI Token' } });
    fireEvent.click(screen.getByRole('button', { name: 'Token erstellen' }));

    expect(await screen.findByText('opaigw_created_secret')).toBeInTheDocument();
    expect(screen.getByText('My CLI Token')).toBeInTheDocument();
  });

  it('disables a token through the row action', async () => {
    renderApp();

    await gotoNav(/API-Tokens/i);
    await screen.findByText('Dev Token');
    fireEvent.click(screen.getByRole('button', { name: 'Deaktivieren' }));

    expect(await screen.findByText('Deaktiviert')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Aktivieren' })).toBeInTheDocument();
  });

  it('deletes a token after confirmation', async () => {
    renderApp();

    await gotoNav(/API-Tokens/i);
    await screen.findByText('Dev Token');
    // row delete opens the confirmation dialog
    fireEvent.click(screen.getByRole('button', { name: 'Löschen' }));
    // confirm inside the dialog (two "Löschen" buttons exist while open — scope to the dialog)
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Löschen' }));

    await waitFor(() => expect(screen.queryByText('Dev Token')).not.toBeInTheDocument());
  });

  it('edits a token name and preserves scopes hidden from the current role', async () => {
    currentRole = 'user';
    renderApp();

    await gotoNav(/API-Tokens/i);
    await screen.findByText('Dev Token');
    fireEvent.click(screen.getByRole('button', { name: 'Bearbeiten' }));
    // Edit opens the same sub-view mask; the name field is a plain "Name" label now.
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'Renamed Dev' } });
    fireEvent.click(screen.getByRole('button', { name: 'Speichern' }));

    expect(await screen.findByText('Renamed Dev')).toBeInTheDocument();
    // the admin scope was hidden from the "user" role's edit form; it must still survive the rename
    expect(screen.getByText('gateway:use, admin')).toBeInTheDocument();
  });

  it('shows usage metadata table from the portal API', async () => {
    renderApp();

    await gotoNav(/Aktivität/i);

    expect(screen.getByRole('heading', { name: 'Aktivität', level: 1 })).toBeInTheDocument();
    // The self-loading Activity view fetches the paged /api/portal/usage envelope
    // and renders the enriched seed row in its default columns (token_name / model /
    // total_tokens / latency_ms). NOTE: server_name (tableHost) is default-hidden
    // in the refactor and owner is scope-gated out of own-scope, so those cells no
    // longer appear here; the "GPU 1" server-cell assertion is dropped accordingly.
    expect(await screen.findByRole('cell', { name: 'qwen-coder' })).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: 'Dev Token' })).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: '8' })).toBeInTheDocument();
    expect(screen.getByRole('cell', { name: '14 ms' })).toBeInTheDocument();
  });

  it('does not fetch the legacy /api/portal/usage endpoint on load', async () => {
    renderApp();
    await screen.findByText(messages.de.welcome);

    const legacyUsageCalls = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      ([path]) => path === '/api/portal/usage',
    ).length;
    expect(legacyUsageCalls).toBe(0);
  });

  it('shows model options from the portal API', async () => {
    renderApp();

    await gotoNav('Modelle');

    expect(await screen.findByRole('heading', { name: 'Modelle', level: 1 })).toBeInTheDocument();
    // One cell: the Model (id) column. (The redundant Name column was removed.)
    expect(screen.getAllByRole('cell', { name: 'qwen-coder' })).toHaveLength(1);
  });

  it("shows each model's API surfaces in the Modelle view", async () => {
    renderApp();

    await gotoNav('Modelle');

    expect(await screen.findByRole('heading', { name: 'Modelle', level: 1 })).toBeInTheDocument();
    expect(screen.getAllByRole('cell', { name: 'qwen-coder' })).toHaveLength(1);
    expect(screen.getByText('anthropic, openai')).toBeInTheDocument();
    expect(screen.getByText('openai')).toBeInTheDocument(); // api-only row's flavors cell
    expect(screen.getByText('anthropic')).toBeInTheDocument(); // anthropic-only row's flavors cell
    // The admin Models view is fed the UNSUPPRESSED (manage) listing, so a hidden
    // model appears here (its id cell) though it never reaches chat.
    expect(screen.getAllByRole('cell', { name: 'hidden-model' })).toHaveLength(1);
  });

  it('hides the model group table while a model detail sub-view is open', async () => {
    renderApp();

    await gotoNav('Modelle');
    await screen.findByRole('heading', { name: 'Modelle', level: 1 });
    // The admin group table (ModelGroupSection) is a sibling below ModelList.
    expect(await screen.findByText(messages.de.modelGroups)).toBeInTheDocument();

    // Open a model's per-server detail sub-view → the group table is hidden.
    fireEvent.click(screen.getAllByRole('button', { name: messages.de.modelDetailsAction })[0]);
    expect(await screen.findByText(messages.de.modelServerTitle)).toBeInTheDocument();
    expect(screen.queryByText(messages.de.modelGroups)).toBeNull();

    // Navigate back → the group table returns.
    fireEvent.click(screen.getByRole('button', { name: messages.de.back }));
    expect(await screen.findByText(messages.de.modelGroups)).toBeInTheDocument();
  });

  it('polls the models list on a ~5s cadence while on the Models view and stops after leaving', async () => {
    renderApp();
    // Settle the authenticated shell + initial load on REAL timers first, so the
    // ConnectionProvider's /healthz interval registers on the real clock (it will
    // not fire during the fake advances below).
    await screen.findByText('Dev User');

    const modelsCalls = () =>
      (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([p]) => p === '/api/portal/models',
      ).length;

    vi.useFakeTimers();
    try {
      // Navigate to Models UNDER fake timers so the poll's setInterval is faked.
      fireEvent.click(screen.getByRole('link', { name: 'Modelle' }));
      // Flush the navigation refetch (loadPortalData) so the baseline is stable —
      // no cadence has elapsed yet, so this counts only the on-navigation fetch.
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByRole('heading', { name: 'Modelle', level: 1 })).toBeInTheDocument();
      const before = modelsCalls();

      // Each ~5s cadence fires exactly one extra models fetch.
      await vi.advanceTimersByTimeAsync(5000);
      expect(modelsCalls()).toBe(before + 1);
      await vi.advanceTimersByTimeAsync(5000);
      expect(modelsCalls()).toBe(before + 2);

      // Leaving the Models view stops the poll — no further models fetches even
      // after several cadences elapse.
      fireEvent.click(screen.getByRole('link', { name: 'Dashboard' }));
      await vi.advanceTimersByTimeAsync(0);
      const afterNav = modelsCalls();
      await vi.advanceTimersByTimeAsync(20000);
      expect(modelsCalls()).toBe(afterNav);
    } finally {
      vi.useRealTimers();
    }
  });

  it('offers only openai-capable models in the chat selector', async () => {
    renderApp();

    await gotoNav(/Chat/i);
    await screen.findByLabelText('Nachricht');
    // The chat model selector is now a searchable Autocomplete: open it and read
    // the options rendered in the listbox portal (scoped by the Modell label — the
    // page also has the "Als Token senden" Autocomplete).
    const modelCombobox = screen.getByRole('combobox', { name: /^(Modell|Model)$/ });
    fireEvent.mouseDown(modelCombobox);
    const optionTexts = (await screen.findAllByRole('option')).map((o) => o.textContent);
    expect(optionTexts).toContain('qwen-coder');
    expect(optionTexts).toContain('api-only');
    expect(optionTexts).not.toContain('anthropic-only');
  });

  it('renders AI servers from the portal API', async () => {
    renderApp();

    await gotoNav('AI Server');

    expect(await screen.findByRole('heading', { name: 'AI Server', level: 1 })).toBeInTheDocument();
    const table = await screen.findByRole('table');
    expect(within(table).getByText('GPU 1')).toBeInTheDocument();
    expect(within(table).getByText('gpu1.example.test')).toBeInTheDocument();
    expect(within(table).getByText('Gesund')).toBeInTheDocument();
    expect(within(table).getByText('Dev User')).toBeInTheDocument();
    expect(screen.queryByText('healthy')).not.toBeInTheDocument();
  });

  it('shows a create form for an admin and creates a server', async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    // The create form is now a sub-view mask: open it, fill the fields, submit.
    expect(screen.getByRole('button', { name: 'Server anlegen' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Server anlegen' }));
    fireEvent.change(await screen.findByLabelText('Name'), { target: { value: 'GPU 2' } });
    fireEvent.change(screen.getByLabelText('Domain'), { target: { value: 'gpu2.example.test' } });
    fireEvent.click(screen.getByRole('button', { name: 'Server anlegen' }));

    expect(await screen.findByText('GPU 2')).toBeInTheDocument();
    expect(screen.getByText('gpu2.example.test')).toBeInTheDocument();
  });

  it('hides the create form and owner picker for a non-admin', async () => {
    currentRole = 'user';
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    expect(screen.queryByRole('button', { name: 'Server anlegen' })).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Besitzer')).not.toBeInTheDocument();

    // Edit opens the sub-view mask; for a non-admin it has no owner picker, and the
    // name field is a plain "Name" label prefilled with the edited server's name.
    fireEvent.click(screen.getByRole('button', { name: 'Bearbeiten' }));
    const nameField = await screen.findByLabelText('Name');
    expect(nameField).toHaveValue('GPU 1');
    expect(screen.queryByLabelText('Besitzer')).not.toBeInTheDocument();
  });

  it('preselects existing owners when an admin edits a server', async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    fireEvent.click(screen.getByRole('button', { name: 'Bearbeiten' }));

    // The owner picker is now a searchable multi-select (chips) inside the edit
    // mask; the existing owner is preselected as a "Dev User" chip. findBy waits
    // for the admin-users fetch that hydrates the picker's options.
    const form = (await screen.findByRole('heading', { name: 'Server bearbeiten' })).closest(
      'section',
    ) as HTMLElement;
    expect(within(form).getByLabelText('Besitzer')).toBeInTheDocument();
    expect(await within(form).findByText('Dev User')).toBeInTheDocument();
  });

  it('sends the unchanged owner selection in the PATCH when an admin edits only the name', async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    fireEvent.click(screen.getByRole('button', { name: 'Bearbeiten' }));
    const form = (await screen.findByRole('heading', { name: 'Server bearbeiten' })).closest(
      'section',
    ) as HTMLElement;
    // Wait for the owner picker to hydrate so the preselected owner is preserved on save.
    await within(form).findByText('Dev User');
    fireEvent.change(within(form).getByLabelText('Name'), { target: { value: 'GPU 1 Renamed' } });
    fireEvent.click(within(form).getByRole('button', { name: 'Speichern' }));

    expect(await screen.findByText('GPU 1 Renamed')).toBeInTheDocument();
    // owners left untouched must survive the rename
    const table = screen.getByRole('table');
    expect(within(table).getByText('Dev User')).toBeInTheDocument();

    const patchCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      ([path, init]) =>
        String(path).startsWith('/api/portal/servers/') &&
        (init as RequestInit | undefined)?.method === 'PATCH',
    );
    expect(patchCall).toBeTruthy();
    const patchBody = JSON.parse((patchCall![1] as RequestInit).body as string);
    expect(patchBody.name).toBe('GPU 1 Renamed');
    // The migrated edit mask always sends the admin's current owner selection;
    // untouched, that equals the existing owner (["usr_dev"]) — owners preserved.
    // (The old "omit owner_ids unless changed" behavior no longer exists.)
    expect(patchBody.owner_ids).toEqual(['usr_dev']);
  });

  it('includes owner_ids in the PATCH when an admin toggles an owner', async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    fireEvent.click(screen.getByRole('button', { name: 'Bearbeiten' }));
    const form = (await screen.findByRole('heading', { name: 'Server bearbeiten' })).closest(
      'section',
    ) as HTMLElement;
    // The single preselected owner shows as a chip; removing it (chip delete)
    // clears the selection, so the PATCH carries owner_ids: [].
    expect(await within(form).findByText('Dev User')).toBeInTheDocument();
    fireEvent.click(within(form).getByTestId('CancelIcon'));
    await waitFor(() => expect(within(form).queryByText('Dev User')).not.toBeInTheDocument());
    fireEvent.click(within(form).getByRole('button', { name: 'Speichern' }));

    await waitFor(() => {
      const patchCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
        ([path, init]) =>
          String(path).startsWith('/api/portal/servers/') &&
          (init as RequestInit | undefined)?.method === 'PATCH',
      );
      expect(patchCall).toBeTruthy();
      const patchBody = JSON.parse((patchCall![1] as RequestInit).body as string);
      expect(patchBody.owner_ids).toEqual([]);
    });
  });

  it('deletes a server after confirmation', async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');
    fireEvent.click(screen.getByRole('button', { name: 'Löschen' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Löschen' }));

    await waitFor(() => expect(screen.queryByText('GPU 1')).not.toBeInTheDocument());
  });

  it("opens a server's applications, creates one, and collapses the drill-down again", async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    fireEvent.click(screen.getByRole('button', { name: 'Anwendungen' }));
    expect(
      await screen.findByRole('heading', { name: 'Anwendungen', level: 2 }),
    ).toBeInTheDocument();

    // Create is a sub-view mask now: open it, fill the port, submit.
    fireEvent.click(screen.getByRole('button', { name: 'Anwendung anlegen' }));
    fireEvent.change(await screen.findByLabelText('Port'), { target: { value: '8000' } });
    fireEvent.click(screen.getByRole('button', { name: 'Anwendung anlegen' }));

    expect(await screen.findByText('http://gpu1.example.test:8000')).toBeInTheDocument();
    const applicationsTable = screen
      .getByText('http://gpu1.example.test:8000')
      .closest('table') as HTMLTableElement;
    expect(within(applicationsTable).getByText('ollama')).toBeInTheDocument();
    expect(within(applicationsTable).getByText('openai, anthropic')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Zurück' }));
    expect(
      screen.queryByRole('heading', { name: 'Anwendungen', level: 2 }),
    ).not.toBeInTheDocument();
  });

  it("opens an application's mappings, creates one, and syncs models to see the summary", async () => {
    apiState.applicationRows = [
      {
        id: 'app_1',
        server_id: 'srv_dev',
        type: 'ollama',
        port: 11434,
        scheme: 'http',
        endpoint: 'http://gpu1.example.test:11434',
        api_flavors: ['openai', 'anthropic'],
        priority: 0,
        weight: 0,
        timeout_ms: 30000,
        affinity_ttl_seconds: 1800,
        admission_queue_timeout_seconds: 0,
        status: 'active',
        always_reachable: false,
        health_check_path: '/v1/health',
        health_check_mode: 'health_path',
        health_check_interval_seconds: 0,
        native_responses: false,
        native_messages: false,
        loaded_models_path: '',
        loaded_models_format: '',
        context_probe_path: '',
        app_path_suffix: '',
        api_token_set: false,
        api_token_header: '',
        benchmark_schedule_enabled: false,
        benchmark_schedule_interval_seconds: 0,
        opportunistic_metrics_enabled: false,
        proxy_listen_port: 0,
        proxy_excluded: false,
        reachable: true,
        last_checked_at: null,
        created_at: '2026-07-11T09:00:00Z',
      },
    ];

    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');
    fireEvent.click(screen.getByRole('button', { name: 'Anwendungen' }));
    await screen.findByText('http://gpu1.example.test:11434');

    fireEvent.click(screen.getByRole('button', { name: 'Modell-Zuordnungen' }));
    expect(
      await screen.findByRole('heading', { name: 'Modell-Zuordnungen', level: 2 }),
    ).toBeInTheDocument();

    // Create is a sub-view mask now: open it, fill both names, submit.
    fireEvent.click(screen.getByRole('button', { name: 'Zuordnung anlegen' }));
    fireEvent.change(await screen.findByLabelText('Gateway-Modellname'), {
      target: { value: 'qwen' },
    });
    fireEvent.change(screen.getByLabelText('Anwendungs-Modellname'), {
      target: { value: 'qwen2.5' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Zuordnung anlegen' }));

    expect(await screen.findByText('qwen')).toBeInTheDocument();
    expect(screen.getByText('qwen2.5')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Modelle abgleichen' }));

    expect(await screen.findByText('Hinzugefügt: 1')).toBeInTheDocument();
    expect(screen.getByText('Deaktiviert: 0')).toBeInTheDocument();
    expect(screen.getByText('Unverändert: 0')).toBeInTheDocument();
    expect(screen.getByText('Konflikte: 0')).toBeInTheDocument();
    expect(screen.getAllByText('synced-model')).toHaveLength(2);
  });

  it('re-fetches portal models after a model sync (onModelsChanged push refresh)', async () => {
    apiState.applicationRows = [
      {
        id: 'app_1',
        server_id: 'srv_dev',
        type: 'ollama',
        port: 11434,
        scheme: 'http',
        endpoint: 'http://gpu1.example.test:11434',
        api_flavors: ['openai', 'anthropic'],
        priority: 0,
        weight: 0,
        timeout_ms: 30000,
        affinity_ttl_seconds: 1800,
        admission_queue_timeout_seconds: 0,
        status: 'active',
        always_reachable: false,
        health_check_path: '/v1/health',
        health_check_mode: 'health_path',
        health_check_interval_seconds: 0,
        native_responses: false,
        native_messages: false,
        loaded_models_path: '',
        loaded_models_format: '',
        context_probe_path: '',
        app_path_suffix: '',
        api_token_set: false,
        api_token_header: '',
        benchmark_schedule_enabled: false,
        benchmark_schedule_interval_seconds: 0,
        opportunistic_metrics_enabled: false,
        proxy_listen_port: 0,
        proxy_excluded: false,
        reachable: true,
        last_checked_at: null,
        created_at: '2026-07-11T09:00:00Z',
      },
    ];

    renderApp();

    const modelsCalls = () =>
      (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([path]) => path === '/api/portal/models',
      ).length;

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');
    fireEvent.click(screen.getByRole('button', { name: 'Anwendungen' }));
    await screen.findByText('http://gpu1.example.test:11434');
    fireEvent.click(screen.getByRole('button', { name: 'Modell-Zuordnungen' }));
    await screen.findByRole('heading', { name: 'Modell-Zuordnungen', level: 2 });

    // Capture the models-fetch count with the drill-down fully open. Opening the
    // drill-down does not change the active view, so nothing else re-fetches
    // models between here and the sync click.
    const before = modelsCalls();
    fireEvent.click(screen.getByRole('button', { name: 'Modelle abgleichen' }));

    // syncModels awaits reload() then calls onModelsChanged, wired through
    // App.refreshPortalDataSilently -> loadPortalData -> api.models(). Without
    // that push refresh the models fetch count would not move.
    await waitFor(() => expect(modelsCalls()).toBeGreaterThan(before));
  });

  it('does not leak a sync summary when switching directly to a sibling application', async () => {
    apiState.applicationRows = [
      {
        id: 'app_1',
        server_id: 'srv_dev',
        type: 'ollama',
        port: 11434,
        scheme: 'http',
        endpoint: 'http://gpu1.example.test:11434',
        api_flavors: ['openai', 'anthropic'],
        priority: 0,
        weight: 0,
        timeout_ms: 30000,
        affinity_ttl_seconds: 1800,
        admission_queue_timeout_seconds: 0,
        status: 'active',
        always_reachable: false,
        health_check_path: '/v1/health',
        health_check_mode: 'health_path',
        health_check_interval_seconds: 0,
        native_responses: false,
        native_messages: false,
        loaded_models_path: '',
        loaded_models_format: '',
        context_probe_path: '',
        app_path_suffix: '',
        api_token_set: false,
        api_token_header: '',
        benchmark_schedule_enabled: false,
        benchmark_schedule_interval_seconds: 0,
        opportunistic_metrics_enabled: false,
        proxy_listen_port: 0,
        proxy_excluded: false,
        reachable: true,
        last_checked_at: null,
        created_at: '2026-07-11T09:00:00Z',
      },
      {
        id: 'app_2',
        server_id: 'srv_dev',
        type: 'vllm',
        port: 8000,
        scheme: 'http',
        endpoint: 'http://gpu1.example.test:8000',
        api_flavors: ['openai'],
        priority: 0,
        weight: 0,
        timeout_ms: 30000,
        affinity_ttl_seconds: 1800,
        admission_queue_timeout_seconds: 0,
        status: 'active',
        always_reachable: false,
        health_check_path: '/v1/health',
        health_check_mode: 'health_path',
        health_check_interval_seconds: 0,
        native_responses: false,
        native_messages: false,
        loaded_models_path: '',
        loaded_models_format: '',
        context_probe_path: '',
        app_path_suffix: '',
        api_token_set: false,
        api_token_header: '',
        benchmark_schedule_enabled: false,
        benchmark_schedule_interval_seconds: 0,
        opportunistic_metrics_enabled: false,
        proxy_listen_port: 0,
        proxy_excluded: false,
        reachable: true,
        last_checked_at: null,
        created_at: '2026-07-11T09:01:00Z',
      },
    ];

    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');
    fireEvent.click(screen.getByRole('button', { name: 'Anwendungen' }));

    // open app_1's mappings and sync to render a summary
    const app1Row = (await screen.findByText('http://gpu1.example.test:11434')).closest(
      'tr',
    ) as HTMLTableRowElement;
    fireEvent.click(within(app1Row).getByRole('button', { name: 'Modell-Zuordnungen' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Modelle abgleichen' }));
    expect(await screen.findByText('Hinzugefügt: 1')).toBeInTheDocument();

    // Mappings are a full sub-view now (one at a time), so reach the sibling via the
    // breadcrumb back to the application list, then open app_2's mappings.
    fireEvent.click(screen.getByRole('button', { name: 'Zurück' }));
    const app2Row = (await screen.findByText('http://gpu1.example.test:8000')).closest(
      'tr',
    ) as HTMLTableRowElement;
    fireEvent.click(within(app2Row).getByRole('button', { name: 'Modell-Zuordnungen' }));

    // the mappings section must remount fresh: app_1's sync summary must be gone
    await waitFor(() => expect(screen.queryByText('Hinzugefügt: 1')).not.toBeInTheDocument());
  });

  it('keeps one back control per drill level and returns to the app list from the mappings', async () => {
    apiState.applicationRows = [
      {
        id: 'app_1',
        server_id: 'srv_dev',
        type: 'ollama',
        port: 11434,
        scheme: 'http',
        endpoint: 'http://gpu1.example.test:11434',
        api_flavors: ['openai', 'anthropic'],
        priority: 0,
        weight: 0,
        timeout_ms: 30000,
        affinity_ttl_seconds: 1800,
        admission_queue_timeout_seconds: 0,
        status: 'active',
        always_reachable: false,
        health_check_path: '/v1/health',
        health_check_mode: 'health_path',
        health_check_interval_seconds: 0,
        native_responses: false,
        native_messages: false,
        loaded_models_path: '',
        loaded_models_format: '',
        context_probe_path: '',
        app_path_suffix: '',
        api_token_set: false,
        api_token_header: '',
        benchmark_schedule_enabled: false,
        benchmark_schedule_interval_seconds: 0,
        opportunistic_metrics_enabled: false,
        proxy_listen_port: 0,
        proxy_excluded: false,
        reachable: true,
        last_checked_at: null,
        created_at: '2026-07-11T09:00:00Z',
      },
    ];

    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    // applications sub-view: exactly one back control (the breadcrumb "Zurück")
    fireEvent.click(screen.getByRole('button', { name: 'Anwendungen' }));
    await screen.findByText('http://gpu1.example.test:11434');
    expect(screen.getAllByRole('button', { name: 'Zurück' })).toHaveLength(1);

    // open the mappings sub-view: still exactly one back control (breadcrumbs
    // replaced the old stacked per-level back headers — one level up at a time).
    fireEvent.click(screen.getByRole('button', { name: 'Modell-Zuordnungen' }));
    expect(
      await screen.findByRole('heading', { name: 'Modell-Zuordnungen', level: 2 }),
    ).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Zurück' })).toHaveLength(1);

    // the back button returns to the application list, not all the way to the servers
    fireEvent.click(screen.getByRole('button', { name: 'Zurück' }));
    expect(
      screen.queryByRole('heading', { name: 'Modell-Zuordnungen', level: 2 }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Anwendungen', level: 2 })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Zurück' })).toHaveLength(1);
  });

  it('generates and revokes a server-reporting-agent token from the server drill-down', async () => {
    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    // The reporting-agent panel is now its own sub-view, reached via the server
    // row's "Server-Reporting-Agent" action (no longer nested under Anwendungen).
    fireEvent.click(screen.getByRole('button', { name: 'Server-Reporting-Agent' }));

    const agentPanel = (
      await screen.findByRole('heading', { name: 'Server-Reporting-Agent', level: 2 })
    ).closest('section') as HTMLElement;
    expect(await within(agentPanel).findByText('Noch kein Token erstellt.')).toBeInTheDocument();

    fireEvent.click(within(agentPanel).getByRole('button', { name: 'Token generieren' }));

    expect(await screen.findByText('opaigw_agent_secret')).toBeInTheDocument();
    expect(within(agentPanel).getByRole('button', { name: 'Token rotieren' })).toBeInTheDocument();

    fireEvent.click(within(agentPanel).getByRole('button', { name: 'Token widerrufen' }));
    const revokeDialog = await screen.findByRole('dialog');
    fireEvent.click(within(revokeDialog).getByRole('button', { name: 'Token widerrufen' }));

    await waitFor(() => expect(screen.queryByText('opaigw_agent_secret')).not.toBeInTheDocument());
    expect(within(agentPanel).getByText('Noch kein Token erstellt.')).toBeInTheDocument();
  });

  it('does not leak a revealed agent-token secret across a server switch', async () => {
    // scope a second server to this test only; adding it to defaultServerRows would
    // break the single-row server tests (getByRole on "Löschen"/"Bearbeiten"/"Anwendungen").
    apiState.serverRows = [
      ...apiState.serverRows,
      {
        id: 'srv_beta',
        name: 'GPU Beta',
        domain: 'gpubeta.example.test',
        server_path_suffix: '',
        status: 'active',
        health_status: 'healthy',
        owners: [],
        last_seen_at: null,
        created_at: '2026-07-11T08:00:00Z',
        netbird_enabled: false,
        netbird_setup_key_id: '',
        netbird_group_id: '',
        netbird_peer_id: '',
        netbird_connected: false,
        netbird_group_ids: [],
        netbird_peer_managed: false,
        netbird_policy_override: '',
        netbird_allow_ping: false,
        netbird_ping_exclude: false,
        agent_status: 'unconfigured',
        agent_presence_timeout_seconds: 0,
        estimated_watts: 0,
        idle_watts: 0,
        price_per_kwh: 0,
        pue: 0,
        price_unit: 'eur_cent',
        admin_groups: [],
        system_group_id: '',
        system_group_name: '',
      },
    ];

    renderApp();

    await gotoNav('AI Server');
    await screen.findByText('GPU 1');

    // open server A (GPU 1) and generate its reporting token
    const rowA = screen.getByText('GPU 1').closest('tr') as HTMLTableRowElement;
    fireEvent.click(within(rowA).getByRole('button', { name: 'Server-Reporting-Agent' }));

    const panelA = (
      await screen.findByRole('heading', { name: 'Server-Reporting-Agent', level: 2 })
    ).closest('section') as HTMLElement;
    fireEvent.click(within(panelA).getByRole('button', { name: 'Token generieren' }));
    expect(await screen.findByText('opaigw_agent_secret')).toBeInTheDocument();

    // collapse the sub-view via the breadcrumb back button
    fireEvent.click(screen.getByRole('button', { name: 'Zurück' }));
    await waitFor(() =>
      expect(
        screen.queryByRole('heading', { name: 'Server-Reporting-Agent', level: 2 }),
      ).not.toBeInTheDocument(),
    );

    // open server B (GPU Beta): its panel remounts fresh and must not show A's secret
    const rowB = screen.getByText('GPU Beta').closest('tr') as HTMLTableRowElement;
    fireEvent.click(within(rowB).getByRole('button', { name: 'Server-Reporting-Agent' }));

    const panelB = (
      await screen.findByRole('heading', { name: 'Server-Reporting-Agent', level: 2 })
    ).closest('section') as HTMLElement;
    expect(await within(panelB).findByText('Noch kein Token erstellt.')).toBeInTheDocument();
    expect(screen.queryByText('opaigw_agent_secret')).not.toBeInTheDocument();
  });

  it('shows the login screen when the session is missing', async () => {
    loggedIn = false;
    renderApp();
    expect(await screen.findByRole('heading', { name: messages.de.signIn })).toBeInTheDocument();
  });

  it('switches the login screen language from the top-right menu', async () => {
    loggedIn = false;
    renderApp();
    await screen.findByRole('heading', { name: messages.de.signIn });
    fireEvent.click(screen.getByRole('button', { name: messages.de.language }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'EN' }));
    expect(await screen.findByRole('heading', { name: messages.en.signIn })).toBeInTheDocument();
  });

  it('does not fetch portal data when switching the login language', async () => {
    loggedIn = false;
    renderApp();
    await screen.findByRole('heading', { name: messages.de.signIn });

    const meCalls = () =>
      (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([path]) => path === '/api/portal/me',
      ).length;
    const before = meCalls();

    fireEvent.click(screen.getByRole('button', { name: messages.de.language }));
    fireEvent.click(await screen.findByRole('menuitem', { name: 'EN' }));
    await screen.findByRole('heading', { name: messages.en.signIn });

    expect(meCalls()).toBe(before);
  });

  it('does not call protected portal endpoints before login', async () => {
    loggedIn = false;
    renderApp();
    await screen.findByRole('heading', { name: messages.de.signIn });
    const protectedCalls = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
      ([path]) => typeof path === 'string' && path.startsWith('/api/portal/'),
    ).length;
    expect(protectedCalls).toBe(0);
  });

  it('signs in and loads the dashboard', async () => {
    loggedIn = false;
    renderApp();
    fireEvent.change(await screen.findByLabelText(messages.de.emailLabel), {
      target: { value: 'dev@example.test' },
    });
    fireEvent.change(screen.getByLabelText(messages.de.passwordLabel), {
      target: { value: 'dev-secret' },
    });
    fireEvent.click(screen.getByRole('button', { name: messages.de.loginButton }));
    expect(await screen.findByText(messages.de.welcome)).toBeInTheDocument();
  });

  it("applies the user's preferred language after form login", async () => {
    loggedIn = false;
    sessionLanguage = 'en'; // system default -> login screen is English
    renderApp();
    await screen.findByRole('heading', { name: messages.en.signIn });
    fireEvent.change(screen.getByLabelText(messages.en.emailLabel), {
      target: { value: 'dev@example.test' },
    });
    fireEvent.change(screen.getByLabelText(messages.en.passwordLabel), {
      target: { value: 'dev-secret' },
    });
    fireEvent.click(screen.getByRole('button', { name: messages.en.loginButton }));
    // devUser.preferred_language is "de" -> the shell switches to German after login.
    expect(await screen.findByText(messages.de.welcome)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: messages.de.language })).toBeInTheDocument();
  });

  it('shows a localized error on the login screen for invalid credentials', async () => {
    loggedIn = false;
    renderApp();
    fireEvent.change(await screen.findByLabelText(messages.de.emailLabel), {
      target: { value: 'dev@example.test' },
    });
    fireEvent.change(screen.getByLabelText(messages.de.passwordLabel), {
      target: { value: 'wrong-password' },
    });
    fireEvent.click(screen.getByRole('button', { name: messages.de.loginButton }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      `auth.invalid_credentials: ${messages.de.errorAuthInvalidCredentials}`,
    );
  });

  it('logs out and returns to the login screen', async () => {
    renderApp();

    await screen.findByText('Dev User');
    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: messages.de.logout }));

    expect(await screen.findByRole('heading', { name: messages.de.signIn })).toBeInTheDocument();
    expect(loggedIn).toBe(false);
  });

  it('moves aria-current to the active navigation view', async () => {
    renderApp();
    await screen.findByText('Dev User');

    await gotoNav('Chat');
    expect(screen.getByRole('link', { name: 'Chat' })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'Dashboard' })).not.toHaveAttribute('aria-current');

    await gotoNav('Aktivität');
    expect(screen.getByRole('link', { name: 'Aktivität' })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'Chat' })).not.toHaveAttribute('aria-current');
  });

  it('refetches portal data when navigating to a view', async () => {
    renderApp();

    const modelsCalls = () =>
      (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([path]) => path === '/api/portal/models',
      ).length;

    // wait for the initial (non-silent) authenticated load to settle
    await screen.findByText('Dev User');
    await waitFor(() => expect(modelsCalls()).toBeGreaterThan(0));

    // navigating to a different view must silently re-fetch the portal data
    const beforeModels = modelsCalls();
    await gotoNav('Modelle');
    await waitFor(() => expect(modelsCalls()).toBeGreaterThan(beforeModels));

    // and navigating back triggers another refetch (the reported bug: it previously did not)
    const beforeBack = modelsCalls();
    await gotoNav('Dashboard');
    await waitFor(() => expect(modelsCalls()).toBeGreaterThan(beforeBack));
  });

  it('lets an admin invite a user and shows the invite link', async () => {
    renderApp();
    await screen.findByText(messages.de.welcome);
    await gotoNav(messages.de.users);
    // Invite opens a sub-view mask: open it, fill the email, submit. The
    // single manageable admin group auto-selects once the async
    // GET /api/portal/groups load resolves, so the submit button starts
    // disabled and must be awaited enabled before the final click.
    fireEvent.click(await screen.findByRole('button', { name: messages.de.userCreate }));
    fireEvent.change(await screen.findByLabelText(messages.de.tableEmail), {
      target: { value: 'new@example.test' },
    });
    await waitFor(() =>
      expect(screen.getByRole('button', { name: messages.de.userCreate })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole('button', { name: messages.de.userCreate }));
    expect(
      await screen.findByText('http://localhost:8080/set-password?token=abc'),
    ).toBeInTheDocument();
  });

  it('changes the password from management', async () => {
    renderApp();
    await screen.findByText(messages.de.welcome);
    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: messages.de.profile }));
    fireEvent.change(await screen.findByLabelText(messages.de.currentPasswordLabel), {
      target: { value: 'dev-secret' },
    });
    fireEvent.change(screen.getByLabelText(messages.de.newPasswordLabel), {
      target: { value: 'password-new' },
    });
    fireEvent.change(screen.getByLabelText(messages.de.confirmPasswordLabel), {
      target: { value: 'password-new' },
    });
    fireEvent.click(screen.getByRole('button', { name: messages.de.changePasswordButton }));
    expect(await screen.findByText(messages.de.changePasswordSuccess)).toBeInTheDocument();
  });

  it('shows the System nav item for a system admin', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    expect(screen.getByRole('link', { name: messages.de.system })).toBeInTheDocument();
  });

  it('re-fetches scope-dependent content when switching System-Admin mode (no stale scope)', async () => {
    // A system_admin session starts NON-elevated (plain-admin scope): the step-up
    // control is shown, but the `system` scope — and thus the System group panel —
    // is withheld until the operator elevates.
    currentRole = 'system_admin';
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.groups);
    // The Admin panel loads (in scope); the System-tier group is out of scope.
    expect(await screen.findByText('Default Admin Group')).toBeInTheDocument();
    expect(screen.queryByText('Global System Group')).not.toBeInTheDocument();

    // Open the user dropdown, then enter System-Admin mode via the step-up item
    // (password confirmation). The control now lives in the user menu.
    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(
      await screen.findByRole('menuitem', { name: messages.de.systemAdminModeEnter }),
    );
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(messages.de.systemAdminModePasswordLabel), {
      target: { value: 'dev-secret' },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: messages.de.systemAdminModeEnter }));

    // Elevation flips the scope: the routed content remounts + re-fetches, so the
    // now-in-scope System group appears. This is the reported bug — it previously
    // kept showing the stale pre-elevation landscape ("zu wenig").
    expect(await screen.findByText('Global System Group')).toBeInTheDocument();

    // Leaving System-Admin mode drops the scope again: reopen the user dropdown
    // and click the now-elevated "Leave" item. `findByRole` for the trigger
    // retries past the confirm dialog's close transition, whose backdrop
    // otherwise aria-hides the header until it finishes. The System group must
    // disappear rather than linger from the elevated fetch ("zu viel").
    fireEvent.click(await screen.findByRole('button', { name: 'Dev User' }));
    fireEvent.click(
      await screen.findByRole('menuitem', { name: messages.de.systemAdminModeLeave }),
    );
    await waitFor(() => expect(screen.queryByText('Global System Group')).not.toBeInTheDocument());
  });

  it('falls back to the Dashboard (instead of a blank main area) when a system-admin-gated view loses its gate', async () => {
    // Regression test for the view-registry refactor (FE-3): the main area
    // used to have NO fallback when the active view's gate dropped mid-visit
    // (e.g. System-Admin mode auto-dropping while `system`/`netbird`/
    // `certificates`/`logs` was the active view) — it silently rendered
    // nothing. `view` state is untouched on de-elevation (no reset-on-drop
    // exists), so the only thing that can save the main area is an explicit
    // fallback at render time. This asserts the fallback: leaving
    // System-Admin mode while on the System view now shows the Dashboard.
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');

    await gotoNav(messages.de.system);
    expect(await screen.findByRole('heading', { name: messages.de.system })).toBeInTheDocument();

    // Leave System-Admin mode (no password confirmation needed to leave).
    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(
      await screen.findByRole('menuitem', { name: messages.de.systemAdminModeLeave }),
    );

    // The System view's gate (systemAdminMode) just dropped; `view` itself
    // stays 'system' (no reset-on-drop), so the fallback is what saves the
    // main area from going blank.
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: messages.de.system })).not.toBeInTheDocument(),
    );
    expect(screen.getByRole('heading', { name: messages.de.dashboard })).toBeInTheDocument();
  });

  it('hides the System nav item from a plain admin', async () => {
    renderApp();
    await screen.findByText('Dev User');
    expect(screen.getByRole('link', { name: messages.de.dashboard })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: messages.de.system })).not.toBeInTheDocument();
  });

  it('hides the System nav item from a regular user', async () => {
    currentRole = 'user';
    renderApp();
    await screen.findByText('Dev User');
    expect(screen.getByRole('link', { name: messages.de.dashboard })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: messages.de.system })).not.toBeInTheDocument();
  });

  it('no longer renders a Policies nav item', async () => {
    currentRole = 'system_admin';
    renderApp();
    await screen.findByText('Dev User');
    expect(screen.queryByText('Policies')).not.toBeInTheDocument();
  });

  it('shows the Tools nav item + view for admin and system_admin, hides it for a regular user', async () => {
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.tools);
    expect(
      await screen.findByRole('heading', { name: messages.de.settingsPingTitle }),
    ).toBeInTheDocument();
  });

  it('hides the Tools nav item from a regular user', async () => {
    currentRole = 'user';
    renderApp();
    await screen.findByText('Dev User');
    expect(screen.queryByRole('link', { name: messages.de.tools })).not.toBeInTheDocument();
  });

  it('shows the NetBird nav item + view only for a system admin when the module is enabled', async () => {
    netbirdModuleEnabledMock = true;
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.settingsNetbirdTitle);
    // Level-1: NetbirdSettings' own PageTitle. (Its own Panel below it reuses the
    // same "NetBird" label for its own h2 subheading — disambiguate.)
    expect(
      await screen.findByRole('heading', { level: 1, name: messages.de.settingsNetbirdTitle }),
    ).toBeInTheDocument();
    // The enable checkbox stays in System Settings, NOT here.
    expect(screen.queryByLabelText(messages.de.settingsNetbirdEnable)).not.toBeInTheDocument();
    // The url/groups/token/test module-config controls now live HERE.
    await screen.findByLabelText(messages.de.settingsNetbirdUrl);
  });

  it('shows the NetBird nav item + view once the enable checkbox is on, even before url/token are configured (module_enabled true, enabled false)', async () => {
    netbirdRawCheckboxMock = true;
    netbirdModuleEnabledMock = false;
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.settingsNetbirdTitle);
    expect(
      await screen.findByRole('heading', { level: 1, name: messages.de.settingsNetbirdTitle }),
    ).toBeInTheDocument();
    // The (unconfigured) module-config fields are reachable to finish setup.
    await screen.findByLabelText(messages.de.settingsNetbirdUrl);
  });

  it('hides the NetBird nav item from a plain admin even when the module is enabled', async () => {
    netbirdModuleEnabledMock = true;
    renderApp();
    await screen.findByText('Dev User');
    expect(
      screen.queryByRole('link', { name: messages.de.settingsNetbirdTitle }),
    ).not.toBeInTheDocument();
  });

  it('hides the NetBird nav item from a system admin when the module is disabled', async () => {
    netbirdModuleEnabledMock = false;
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    expect(
      screen.queryByRole('link', { name: messages.de.settingsNetbirdTitle }),
    ).not.toBeInTheDocument();
  });

  it('shows ONLY the NetBird enable checkbox inside the System view (always reachable — this is the fix for the chicken-and-egg bug: the nav item is gated on the checkbox being on, so the checkbox itself must live somewhere always reachable)', async () => {
    netbirdModuleEnabledMock = false;
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.system);
    await screen.findByRole('combobox', { name: messages.de.systemThemeLabel });
    expect(
      await screen.findByRole('heading', { name: messages.de.settingsNetbirdTitle }),
    ).toBeInTheDocument();
    // Only the enable checkbox is present — a system admin can flip it on even
    // when the NetBird nav item itself is hidden.
    await screen.findByLabelText(messages.de.settingsNetbirdEnable);
    expect(screen.queryByLabelText(messages.de.settingsNetbirdUrl)).not.toBeInTheDocument();
    // The url/groups/token/test module-config controls AND the OPERATIONAL
    // NetBird settings (netbird_only, policy management, …) stay out of the
    // System view — they live only in the separate NetbirdSettings view.
    expect(screen.queryByLabelText(messages.de.settingsNetbirdOnly)).not.toBeInTheDocument();
  });

  it('reveals the NetBird nav item live after toggling the enable checkbox on and saving (no manual refresh)', async () => {
    netbirdRawCheckboxMock = false;
    netbirdModuleEnabledMock = false;
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');

    // The NetBird nav link starts hidden (module disabled).
    expect(
      screen.queryByRole('link', { name: messages.de.settingsNetbirdTitle }),
    ).not.toBeInTheDocument();

    await gotoNav(messages.de.system);
    // Flip the enable checkbox on and save.
    const enable = await screen.findByLabelText(messages.de.settingsNetbirdEnable);
    fireEvent.click(enable);
    fireEvent.click(screen.getByRole('button', { name: messages.de.save }));
    await screen.findByText(messages.de.systemSaved);

    // The save triggers a re-fetch of the module flag (onSaved) — the nav link
    // appears WITHOUT navigating away or reloading the page.
    expect(
      await screen.findByRole('link', { name: messages.de.settingsNetbirdTitle }),
    ).toBeInTheDocument();
  });

  it('uses the system default language on the login screen', async () => {
    loggedIn = false;
    sessionLanguage = 'en';
    renderApp();
    expect(await screen.findByRole('heading', { name: messages.en.signIn })).toBeInTheDocument();
  });

  it('shows the language selection in system settings and profile', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');

    await gotoNav(messages.de.system);
    // The language select is now a non-native MUI Select: its combobox shows the
    // selected option's label text ("Deutsch"); pick another by opening + clicking.
    const languageSelect = await screen.findByRole('combobox', {
      name: messages.de.systemLanguageLabel,
    });
    expect(languageSelect).toBeInTheDocument();
    expect(languageSelect).toHaveTextContent('Deutsch');

    fireEvent.mouseDown(languageSelect);
    fireEvent.click(await screen.findByRole('option', { name: 'English' }));
    fireEvent.click(screen.getByRole('button', { name: messages.de.save }));
    await screen.findByText(messages.de.systemSaved);

    const putCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      ([path, init]) =>
        path === '/api/system/settings' && (init as RequestInit | undefined)?.method === 'PUT',
    );
    expect(JSON.parse((putCall![1] as RequestInit).body as string)).toEqual({
      theme: 'default',
      language: 'en',
      capture_retention_days: 30,
      capture_enabled: true,
      capture_override: false,
      health_check_interval_seconds: 30,
      agent_presence_timeout_seconds: 15,
      smtp_enabled: false,
      smtp_host: '',
      smtp_port: 587,
      smtp_username: '',
      smtp_from: '',
      smtp_from_name: '',
      smtp_tls_mode: 'starttls',
      totp_mode: 'off',
      route_affinity_session_mode: 'client_session',
      vision_probe_mode: 'accept',
      energy_default_price_per_kwh: 0,
      energy_default_price_unit: 'eur_cent',
      currency_usd_per_eur: 0,
      energy_default_pue: 0,
      energy_default_wh_per_token: 0,
      netbird_enabled: false,
      system_admin_mode_require_password: true,
      resource_provisioning_enforce: false,
      cert_enabled: false,
    });
  });

  it('persists a profile language change through the portal API', async () => {
    renderApp();
    await screen.findByText(messages.de.welcome);
    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: messages.de.profile }));

    // Non-native MUI Select: the combobox shows "Deutsch"; switch by opening + clicking.
    const profileLanguage = await screen.findByRole('combobox', {
      name: messages.de.profileLanguageLabel,
    });
    expect(profileLanguage).toHaveTextContent('Deutsch');
    fireEvent.mouseDown(profileLanguage);
    fireEvent.click(await screen.findByRole('option', { name: 'English' }));

    // The UI switches immediately …
    expect(await screen.findByRole('button', { name: 'Privacy' })).toBeInTheDocument();
    // … and the choice is persisted via PUT /api/portal/language.
    await waitFor(() => {
      const putCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
        ([path, init]) =>
          path === '/api/portal/language' && (init as RequestInit | undefined)?.method === 'PUT',
      );
      expect(putCall).toBeTruthy();
      expect(JSON.parse((putCall![1] as RequestInit).body as string)).toEqual({ language: 'en' });
    });
  });

  it("initializes the shell locale from the user's preferred language", async () => {
    devUser.preferred_language = 'en';
    try {
      renderApp();
      expect(await screen.findByRole('button', { name: 'Privacy' })).toBeInTheDocument();
    } finally {
      devUser.preferred_language = 'de';
    }
  });

  it('lets a system admin load and save the portal theme', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.system);

    // Non-native MUI Select: the combobox shows the selected theme's display
    // name ("Default"), not its id ("default") — available_themes is [{id,name}].
    const select = await screen.findByRole('combobox', { name: messages.de.systemThemeLabel });
    expect(select).toBeInTheDocument();
    expect(select).toHaveTextContent('Default');

    fireEvent.click(screen.getByRole('button', { name: messages.de.save }));

    expect(await screen.findByText(messages.de.systemSaved)).toBeInTheDocument();

    const putCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      ([path, init]) =>
        path === '/api/system/settings' && (init as RequestInit | undefined)?.method === 'PUT',
    );
    expect(putCall).toBeTruthy();
    expect(JSON.parse((putCall![1] as RequestInit).body as string)).toEqual({
      theme: 'default',
      language: 'de',
      capture_retention_days: 30,
      capture_enabled: true,
      capture_override: false,
      health_check_interval_seconds: 30,
      agent_presence_timeout_seconds: 15,
      smtp_enabled: false,
      smtp_host: '',
      smtp_port: 587,
      smtp_username: '',
      smtp_from: '',
      smtp_from_name: '',
      smtp_tls_mode: 'starttls',
      totp_mode: 'off',
      route_affinity_session_mode: 'client_session',
      vision_probe_mode: 'accept',
      energy_default_price_per_kwh: 0,
      energy_default_price_unit: 'eur_cent',
      currency_usd_per_eur: 0,
      energy_default_pue: 0,
      energy_default_wh_per_token: 0,
      netbird_enabled: false,
      system_admin_mode_require_password: true,
      resource_provisioning_enforce: false,
      cert_enabled: false,
    });
  });

  it('lets a system admin change the capture retention setting', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.system);

    const retention = (await screen.findByLabelText(
      messages.de.captureRetentionLabel,
    )) as HTMLInputElement;
    expect(retention.value).toBe('30');

    fireEvent.change(retention, { target: { value: '14' } });
    fireEvent.click(screen.getByRole('button', { name: messages.de.save }));
    await screen.findByText(messages.de.systemSaved);

    const putCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      ([path, init]) =>
        path === '/api/system/settings' && (init as RequestInit | undefined)?.method === 'PUT',
    );
    expect(putCall).toBeTruthy();
    expect(JSON.parse((putCall![1] as RequestInit).body as string)).toEqual({
      theme: 'default',
      language: 'de',
      capture_retention_days: 14,
      capture_enabled: true,
      capture_override: false,
      health_check_interval_seconds: 30,
      agent_presence_timeout_seconds: 15,
      smtp_enabled: false,
      smtp_host: '',
      smtp_port: 587,
      smtp_username: '',
      smtp_from: '',
      smtp_from_name: '',
      smtp_tls_mode: 'starttls',
      totp_mode: 'off',
      route_affinity_session_mode: 'client_session',
      vision_probe_mode: 'accept',
      energy_default_price_per_kwh: 0,
      energy_default_price_unit: 'eur_cent',
      currency_usd_per_eur: 0,
      energy_default_pue: 0,
      energy_default_wh_per_token: 0,
      netbird_enabled: false,
      system_admin_mode_require_password: true,
      resource_provisioning_enforce: false,
      cert_enabled: false,
    });
  });

  it('disables save when the capture retention is out of range', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.system);

    const retention = (await screen.findByLabelText(
      messages.de.captureRetentionLabel,
    )) as HTMLInputElement;

    fireEvent.change(retention, { target: { value: '0' } });
    expect(screen.getByRole('button', { name: messages.de.save })).toBeDisabled();

    fireEvent.change(retention, { target: { value: '366' } });
    expect(screen.getByRole('button', { name: messages.de.save })).toBeDisabled();

    fireEvent.change(retention, { target: { value: '30' } });
    expect(screen.getByRole('button', { name: messages.de.save })).not.toBeDisabled();
  });

  it('lets a system admin toggle the capture_enabled kill switch', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.system);

    const toggle = (await screen.findByLabelText(
      messages.de.captureEnabledLabel,
    )) as HTMLInputElement;
    expect(toggle.checked).toBe(true);

    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole('button', { name: messages.de.save }));
    await screen.findByText(messages.de.systemSaved);

    const putCall = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.find(
      ([path, init]) =>
        path === '/api/system/settings' && (init as RequestInit | undefined)?.method === 'PUT',
    );
    expect(putCall).toBeTruthy();
    expect(JSON.parse((putCall![1] as RequestInit).body as string)).toEqual({
      theme: 'default',
      language: 'de',
      capture_retention_days: 30,
      capture_enabled: false,
      capture_override: false,
      health_check_interval_seconds: 30,
      agent_presence_timeout_seconds: 15,
      smtp_enabled: false,
      smtp_host: '',
      smtp_port: 587,
      smtp_username: '',
      smtp_from: '',
      smtp_from_name: '',
      smtp_tls_mode: 'starttls',
      totp_mode: 'off',
      route_affinity_session_mode: 'client_session',
      vision_probe_mode: 'accept',
      energy_default_price_per_kwh: 0,
      energy_default_price_unit: 'eur_cent',
      currency_usd_per_eur: 0,
      energy_default_pue: 0,
      energy_default_wh_per_token: 0,
      netbird_enabled: false,
      system_admin_mode_require_password: true,
      resource_provisioning_enforce: false,
      cert_enabled: false,
    });
  });

  it('re-applies the theme (re-fetches GET /api/system/theme) after a save', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.system);
    await screen.findByLabelText(messages.de.systemThemeLabel);

    const themeFetches = () =>
      (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.filter(
        ([path]) => path === '/api/system/theme',
      ).length;

    const before = themeFetches();
    fireEvent.click(screen.getByRole('button', { name: messages.de.save }));
    await screen.findByText(messages.de.systemSaved);

    // SystemSettings.save() calls reloadTheme() on success, which re-fetches the
    // public theme and re-applies it — the "apply immediately" wiring.
    await waitFor(() => expect(themeFetches()).toBeGreaterThan(before));
  });

  it('offers the system-admin role option in the user form only to a system admin', async () => {
    currentRole = 'system_admin';
    systemAdminModeMock = true;
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.users);

    // The role select lives in the create sub-view mask; open it first.
    fireEvent.click(await screen.findByRole('button', { name: messages.de.userCreate }));
    // The role select is a non-native MUI Select: open it and read the options
    // rendered in the listbox portal.
    const roleSelect = await screen.findByRole('combobox', { name: messages.de.tableRole });
    fireEvent.mouseDown(roleSelect);
    const optionTexts = (await screen.findAllByRole('option')).map((option) => option.textContent);
    expect(optionTexts).toContain(messages.de.roleSystemAdmin);
  });

  it('hides the system-admin role option in the user form from a plain admin', async () => {
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.users);

    // The role select lives in the create sub-view mask; open it first.
    fireEvent.click(await screen.findByRole('button', { name: messages.de.userCreate }));
    // The role select is a non-native MUI Select: open it and read the options
    // rendered in the listbox portal.
    const roleSelect = await screen.findByRole('combobox', { name: messages.de.tableRole });
    fireEvent.mouseDown(roleSelect);
    const optionTexts = (await screen.findAllByRole('option')).map((option) => option.textContent);
    expect(optionTexts).not.toContain(messages.de.roleSystemAdmin);
  });

  it('labels a system_admin user in the users table', async () => {
    // scope the extra row to this test so single-user fixtures (owner pickers) stay intact
    apiState.userRows = [
      ...apiState.userRows,
      {
        id: 'usr_sys',
        email: 'sys@example.test',
        display_name: 'Sys Admin',
        role: 'system_admin',
        status: 'active',
        preferred_language: 'de',
        created_at: '2026-07-10T12:00:00Z',
      },
    ];
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.users);

    const row = (await screen.findByText('sys@example.test')).closest('tr') as HTMLTableRowElement;
    expect(within(row).getByText(messages.de.roleSystemAdmin)).toBeInTheDocument();
  });

  it('keeps admin server management (create form) for a system admin', async () => {
    currentRole = 'system_admin';
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.servers);
    await screen.findByText('GPU 1');
    // system_admin must retain the same create-server control an admin sees
    expect(screen.getByRole('button', { name: messages.de.serverCreate })).toBeInTheDocument();
  });

  it('offers the admin token scope to a system admin', async () => {
    currentRole = 'system_admin';
    renderApp();
    await screen.findByText('Dev User');
    await gotoNav(messages.de.apiTokens);
    await screen.findByText('Dev Token');
    // The scope checkboxes live in the create sub-view mask; open it first.
    fireEvent.click(screen.getByRole('button', { name: messages.de.tokenCreate }));
    // system_admin must be able to pick the admin scope, not just gateway:use
    expect(await screen.findByLabelText('admin')).toBeInTheDocument();
  });

  it('keeps German and English message keys in parity for shell strings', () => {
    const deKeys = Object.keys(messages.de).sort();
    const enKeys = Object.keys(messages.en).sort();
    // TS already enforces de/en key parity at compile time (`en: PortalMessages`,
    // `PortalMessages = typeof de`); this test guards the runtime shape too.
    expect(enKeys).toEqual(deKeys);

    // Every value must be a string or a function (never undefined/null/wrong
    // type). Not all strings are required to be non-empty: e.g.
    // activityTsUnitReqPerSec is intentionally '' for unitless series (see
    // i18n.test.ts's "labelled (non-unit) keys are always non-empty" note).
    for (const locale of ['de', 'en'] as const) {
      for (const [key, value] of Object.entries(messages[locale])) {
        expect(value, `${locale}.${key} should be a string or a function`).toSatisfy(
          (v) => typeof v === 'string' || typeof v === 'function',
        );
      }
    }
  });
});
