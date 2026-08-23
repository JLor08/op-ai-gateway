// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { buildQueryString, createPortalApi } from './api';

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('portal api', () => {
  it('sends cookies and parses dashboard response', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          metrics: { requests_24h: 2, tokens_24h: 40, healthy_hosts: 'mock', latency_p95_ms: 30 },
          routes: [{ model: 'qwen-coder', provider: 'mock', host: 'mock-host', status: 'active' }],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const dashboard = await api.dashboard();

    expect(fetcher).toHaveBeenCalledWith('/api/portal/dashboard', {
      headers: {},
      credentials: 'include',
    });
    expect(dashboard.metrics.requests_24h).toBe(2);
  });

  it('sends the CSRF header on mutating requests', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'u',
          email: 'e',
          display_name: 'd',
          role: 'admin',
          preferred_language: 'de',
        }),
        {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        },
      ),
    );
    const api = createPortalApi(fetcher);

    await api.login('e@example.test', 'password-1');

    const [, init] = fetcher.mock.calls[0];
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.headers['Content-Type']).toBe('application/json');
  });

  it('throws stable PortalApiError for API errors', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({ error: { code: 'auth.session_invalid', message: 'session is invalid' } }),
        {
          status: 401,
          headers: { 'Content-Type': 'application/json' },
        },
      ),
    );
    const api = createPortalApi(fetcher);

    await expect(api.me()).rejects.toMatchObject({
      status: 401,
      code: 'auth.session_invalid',
    });
  });

  it('throws stable PortalApiError for non-JSON API failures', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response('<html>bad gateway</html>', {
        status: 502,
        statusText: 'Bad Gateway',
        headers: { 'Content-Type': 'text/html' },
      }),
    );
    const api = createPortalApi(fetcher);

    await expect(api.me()).rejects.toMatchObject({ status: 502, code: 'request.failed' });
  });

  it('updates a token via PATCH with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'tok_1',
          name: 'Renamed',
          secret_prefix: 'opaigw_',
          status: 'disabled',
          scopes: ['gateway:use'],
          expires_at: null,
          last_used_at: null,
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const token = await api.updateToken('tok_1', { name: 'Renamed', status: 'disabled' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/tokens/tok_1');
    expect(init.method).toBe('PATCH');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(token.status).toBe('disabled');
  });

  it('deletes a token via DELETE', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.deleteToken('tok_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/tokens/tok_1');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
  });

  it('rotates a token via POST and parses the new secret', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          token: {
            id: 'tok_1',
            name: 'Dev Token',
            secret_prefix: 'opaigw_new',
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
          },
          secret: 'opaigw_rotated_secret',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const res = await api.rotateToken('tok_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/tokens/tok_1/rotate');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(res.secret).toBe('opaigw_rotated_secret');
    expect(res.token.secret_prefix).toBe('opaigw_new');
  });

  it('lists AI servers', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: [
            {
              id: 'srv_1',
              name: 'GPU 1',
              domain: 'gpu1.example.test',
              status: 'active',
              health_status: 'healthy',
              owners: [{ id: 'usr_dev', email: 'dev@example.test', display_name: 'Dev User' }],
              last_seen_at: null,
              created_at: '2026-07-11T00:00:00Z',
            },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const response = await api.servers();

    expect(fetcher).toHaveBeenCalledWith('/api/portal/servers', {
      headers: {},
      credentials: 'include',
    });
    expect(response.data[0].domain).toBe('gpu1.example.test');
  });

  it('creates an AI server via POST with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'srv_1',
          name: 'GPU 1',
          domain: 'gpu1.example.test',
          status: 'active',
          health_status: 'unknown',
          owners: [],
          last_seen_at: null,
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const server = await api.createServer({
      name: 'GPU 1',
      domain: 'gpu1.example.test',
      owner_ids: ['usr_dev'],
      admin_group_ids: ['ag_1'],
    });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/servers');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({
      name: 'GPU 1',
      domain: 'gpu1.example.test',
      owner_ids: ['usr_dev'],
      admin_group_ids: ['ag_1'],
    });
    expect(server.domain).toBe('gpu1.example.test');
  });

  it('updates an AI server via PATCH with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'srv_1',
          name: 'GPU One',
          domain: 'gpu1.example.test',
          status: 'disabled',
          health_status: 'unknown',
          owners: [],
          last_seen_at: null,
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const server = await api.updateServer('srv_1', { name: 'GPU One', status: 'disabled' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/servers/srv_1');
    expect(init.method).toBe('PATCH');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(server.status).toBe('disabled');
  });

  it('deletes an AI server via DELETE', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.deleteServer('srv_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/servers/srv_1');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
  });

  it('threads ?delete_peer=true when deleting a server with peer cleanup', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true, netbird_peer_delete_failed: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const res = await api.deleteServer('srv_1', true);

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/servers/srv_1?delete_peer=true');
    expect(init.method).toBe('DELETE');
    expect(res.netbird_peer_delete_failed).toBe(true);
  });

  it('lists applications for a server', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: [
            {
              id: 'app_1',
              server_id: 'srv_1',
              type: 'vllm',
              port: 8000,
              scheme: 'https',
              endpoint: 'https://gpu1.example.test:8000',
              api_flavors: ['openai'],
              priority: 0,
              weight: 0,
              timeout_ms: 30000,
              affinity_ttl_seconds: 1800,
              status: 'active',
              reachable: true,
              last_checked_at: '2026-07-11T00:00:00Z',
              created_at: '2026-07-11T00:00:00Z',
            },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const response = await api.applications('srv_1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/servers/srv_1/applications', {
      headers: {},
      credentials: 'include',
    });
    expect(response.data[0].type).toBe('vllm');
  });

  it('creates an application via POST with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'app_1',
          server_id: 'srv_1',
          type: 'vllm',
          port: 8000,
          scheme: 'https',
          endpoint: 'https://gpu1.example.test:8000',
          api_flavors: ['openai'],
          priority: 0,
          weight: 0,
          timeout_ms: 30000,
          affinity_ttl_seconds: 1800,
          status: 'active',
          reachable: true,
          last_checked_at: '2026-07-11T00:00:00Z',
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const app = await api.createApplication('srv_1', {
      type: 'vllm',
      port: 8000,
      scheme: 'https',
      api_flavors: ['openai'],
      priority: 0,
      weight: 0,
      timeout_ms: 30000,
      affinity_ttl_seconds: 1800,
      status: 'active',
    });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/servers/srv_1/applications');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({
      type: 'vllm',
      port: 8000,
      scheme: 'https',
      api_flavors: ['openai'],
      priority: 0,
      weight: 0,
      timeout_ms: 30000,
      affinity_ttl_seconds: 1800,
      status: 'active',
    });
    expect(app.endpoint).toBe('https://gpu1.example.test:8000');
  });

  it('fetches a single application via GET', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'app_1',
          server_id: 'srv_1',
          type: 'vllm',
          port: 8000,
          scheme: 'https',
          endpoint: 'https://gpu1.example.test:8000',
          api_flavors: ['openai'],
          priority: 0,
          weight: 0,
          timeout_ms: 30000,
          affinity_ttl_seconds: 1800,
          status: 'active',
          reachable: true,
          last_checked_at: '2026-07-11T00:00:00Z',
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const app = await api.application('app_1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/applications/app_1', {
      headers: {},
      credentials: 'include',
    });
    expect(app.id).toBe('app_1');
  });

  it('updates an application via PATCH with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'app_1',
          server_id: 'srv_1',
          type: 'vllm',
          port: 8001,
          scheme: 'https',
          endpoint: 'https://gpu1.example.test:8001',
          api_flavors: ['openai'],
          priority: 0,
          weight: 0,
          timeout_ms: 30000,
          affinity_ttl_seconds: 1800,
          status: 'disabled',
          reachable: false,
          last_checked_at: '2026-07-11T00:00:00Z',
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const app = await api.updateApplication('app_1', { port: 8001, status: 'disabled' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/applications/app_1');
    expect(init.method).toBe('PATCH');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(app.status).toBe('disabled');
  });

  it('deletes an application via DELETE', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.deleteApplication('app_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/applications/app_1');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
  });

  it('syncs application models via POST with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ added: 2, disabled: 0, unchanged: 0, conflicted: 0 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const result = await api.syncApplicationModels('app_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/applications/app_1/sync-models');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(result).toEqual({ added: 2, disabled: 0, unchanged: 0, conflicted: 0 });
  });

  it('lists mappings for an application', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: [
            {
              id: 'map_1',
              application_id: 'app_1',
              gateway_model_name: 'qwen',
              app_model_name: 'qwen2.5',
              status: 'active',
              created_at: '2026-07-11T00:00:00Z',
            },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const response = await api.mappings('app_1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/applications/app_1/mappings', {
      headers: {},
      credentials: 'include',
    });
    expect(response.data[0].gateway_model_name).toBe('qwen');
  });

  it('creates a mapping via POST with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'map_1',
          application_id: 'app_1',
          gateway_model_name: 'qwen',
          app_model_name: 'qwen2.5',
          status: 'active',
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 201, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const mapping = await api.createMapping('app_1', {
      gateway_model_name: 'qwen',
      app_model_name: 'qwen2.5',
      status: 'active',
    });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/applications/app_1/mappings');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({
      gateway_model_name: 'qwen',
      app_model_name: 'qwen2.5',
      status: 'active',
    });
    expect(mapping.app_model_name).toBe('qwen2.5');
  });

  it('updates a mapping via PATCH with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'map_1',
          application_id: 'app_1',
          gateway_model_name: 'qwen',
          app_model_name: 'qwen2.5',
          status: 'disabled',
          created_at: '2026-07-11T00:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const mapping = await api.updateMapping('map_1', { status: 'disabled' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/mappings/map_1');
    expect(init.method).toBe('PATCH');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(mapping.status).toBe('disabled');
  });

  it('deletes a mapping via DELETE', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.deleteMapping('map_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/mappings/map_1');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
  });

  it('fetches the paged activity list with a query string and no CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ data: [], page: 2, limit: 50, total: 0, total_pages: 0 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const page = await api.activity({
      page: 2,
      limit: 50,
      status: 'error',
      q: 'a b',
      scope: 'all',
    });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage?page=2&limit=50&status=error&q=a+b&scope=all');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(page.page).toBe(2);
  });

  it('fetches usage stats with the same query params', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          totals: {
            total_requests: 0,
            error_count: 0,
            cached_tokens: 0,
            input_tokens: 0,
            output_tokens: 0,
          },
          prompt_per_second: { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 },
          tokens_per_second: { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 },
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const stats = await api.activityStats({ range: '7d', model: 'qwen' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/stats?range=7d&model=qwen');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(stats.totals.total_requests).toBe(0);
  });

  it('fetches the usage time-series with window/bucket/scope query and no CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          points: [
            {
              t: '2026-07-16T12:00:00Z',
              connections: 3,
              concurrency: 1,
              prompt_tokens_per_second: 4.5,
              completion_tokens_per_second: 9,
            },
          ],
          bucket_seconds: 5,
          from: '2026-07-16T11:55:00Z',
          to: '2026-07-16T12:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const ts = await api.usageTimeSeries({ window: '15m', bucket: 10, scope: 'all' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/timeseries?window=15m&bucket=10&scope=all');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(ts.bucket_seconds).toBe(5);
    expect(ts.points[0].connections).toBe(3);
    expect(ts.points[0].prompt_tokens_per_second).toBe(4.5);
  });

  it('fetches the active (in-flight) requests with the scope query and no CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          data: [
            {
              id: 'req_live',
              user_id: 'usr_1',
              token_id: 'tok_1',
              token_name: 'Dev Token',
              model: 'qwen-coder',
              api_flavor: 'openai',
              req_path: '/v1/chat/completions',
              stream: true,
              started_at: '2026-07-16T12:00:00Z',
            },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const resp = await api.activeRequests('all');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/active?scope=all');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(resp.data[0].id).toBe('req_live');
    expect(resp.data[0].stream).toBe(true);
  });

  it('threads user_id/token_id into the activity list query (chat sentinel passes through)', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ data: [], page: 1, limit: 25, total: 0, total_pages: 0 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.activity({ scope: 'all', user_id: 'usr_9', token_id: '__none__' });

    const [path] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage?scope=all&user_id=usr_9&token_id=__none__');
  });

  it('threads user_id/token_id into the time-series query', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ points: [], bucket_seconds: 5, from: '', to: '' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.usageTimeSeries({
      window: '15m',
      bucket: 10,
      scope: 'all',
      user_id: 'usr_9',
      token_id: '__none__',
    });

    const [path] = fetcher.mock.calls[0];
    expect(path).toBe(
      '/api/portal/usage/timeseries?window=15m&bucket=10&scope=all&user_id=usr_9&token_id=__none__',
    );
  });

  it('threads the server filter into the time-series query', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ points: [], bucket_seconds: 5, from: '', to: '' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.usageTimeSeries({ window: '15m', bucket: 10, scope: 'all', server: 'host_x' });

    const [path] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/timeseries?window=15m&bucket=10&scope=all&server=host_x');
  });

  it('fetches the decimated server perf history window', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          points: [
            {
              t: '2026-07-19T12:00:00Z',
              cpu_util_pct: 42,
              mem_used_bytes: 8,
              mem_total_bytes: 16,
              swap_used_bytes: 0,
              swap_total_bytes: 0,
              load1: 1.2,
              load5: 1.1,
              load15: 1,
              active_requests: 2,
              queue_depth: 0,
              gpus: [],
              net: [],
            },
          ],
          from: '2026-07-19T11:45:00Z',
          to: '2026-07-19T12:00:00Z',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const history = await api.serverPerfHistory('srv 1', '15m');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/servers/srv%201/perf?window=15m');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(history.points).toHaveLength(1);
    expect(history.points[0].cpu_util_pct).toBe(42);
    expect(history.from).toBe('2026-07-19T11:45:00Z');
  });

  it('fetches the system log snapshot', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          records: [
            {
              t: '2026-07-22T12:00:00Z',
              level: 'INFO',
              msg: 'hello',
              attrs: { server_id: 'srv_1' },
            },
          ],
          level: 'info',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const res = await api.logs();

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/system/logs');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(res.level).toBe('info');
    expect(res.records).toHaveLength(1);
    expect(res.records[0].msg).toBe('hello');
    expect(res.records[0].attrs?.server_id).toBe('srv_1');
  });

  it('sets the log level via a PUT with the level body', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ level: 'debug' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const res = await api.setLogLevel('debug');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/system/logs/level');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({ level: 'debug' });
    expect(res.level).toBe('debug');
  });

  it('threads user_id/token_id into the active-requests query via the params arg', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ data: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.activeRequests('all', { user_id: 'usr_9', token_id: 'tok_5' });

    const [path] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/active?scope=all&user_id=usr_9&token_id=tok_5');
  });

  it("lists a specific user's tokens via the admin endpoint with no CSRF header", async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ data: [{ id: 'chat-session', is_chat_session: true }] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const resp = await api.userTokens('usr_42');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/admin/users/usr_42/tokens');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(resp.data[0].id).toBe('chat-session');
  });

  it('fetches a capture detail by id and sends no CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'req_1',
          api_flavor: 'openai_chat_completions',
          http_status: 200,
          created_at: '2026-07-10T12:00:00Z',
          req_headers: { 'Content-Type': ['application/json'] },
          req_body: '{"model":"m"}',
          resp_headers: { 'Content-Type': ['application/json'] },
          resp_body: '{"choices":[]}',
          truncated: false,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const detail = await api.captureDetail('req_1');

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/captures/req_1');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBeUndefined();
    expect(detail.api_flavor).toBe('openai_chat_completions');
    expect(detail.http_status).toBe(200);
    expect(detail.truncated).toBe(false);
  });

  it('toggles a capture secret flag via PATCH with a CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const result = await api.setCaptureSecret('req_1', true);

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/usage/captures/req_1');
    expect(init.method).toBe('PATCH');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({ secret: true });
    expect(result.ok).toBe(true);
  });

  it('updates the token-less chat settings via PUT with a CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'u',
          email: 'e',
          display_name: 'd',
          role: 'admin',
          preferred_language: 'de',
        }),
        {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        },
      ),
    );
    const api = createPortalApi(fetcher);

    const user = await api.updateChatSettings({ log_communication: true, secret: true });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/chat-settings');
    expect(init.method).toBe('PUT');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({ log_communication: true, secret: true });
    expect(user.id).toBe('u');
  });

  it('fires a keepalive PUT with a CSRF header for saveChatKeepalive and swallows errors', async () => {
    const fetcher = vi.fn().mockRejectedValue(new Error('network error'));
    const api = createPortalApi(fetcher);

    expect(() =>
      api.saveChatKeepalive('c_1', { title: 'hello', content: { messages: [] } }),
    ).not.toThrow();

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/chats/c_1');
    expect(init.method).toBe('PUT');
    expect(init.keepalive).toBe(true);
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(init.body as string)).toEqual({
      title: 'hello',
      content: { messages: [] },
    });

    // Give the rejected fetch promise's .catch(() => {}) a turn to run so an
    // unhandled-rejection failure would surface here rather than in a later test.
    await Promise.resolve();
    await Promise.resolve();
  });

  it('updates system settings via PUT and forwards smtp fields', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          theme: 'default',
          available_themes: [{ id: 'default', name: 'Default' }],
          language: 'de',
          available_languages: ['de'],
          capture_retention_days: 30,
          capture_enabled: true,
          capture_override: false,
          health_check_interval_seconds: 30,
          smtp_enabled: true,
          smtp_host: 'smtp.example.test',
          smtp_port: 587,
          smtp_username: 'user',
          smtp_password_set: true,
          smtp_from: 'noreply@example.test',
          smtp_from_name: 'OP AI Gateway',
          smtp_tls_mode: 'starttls',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    const settings = await api.updateSystemSettings({
      smtp_enabled: true,
      smtp_port: 587,
      smtp_password: 'sec',
    });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/system/settings');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({
      smtp_enabled: true,
      smtp_port: 587,
      smtp_password: 'sec',
    });
    expect(settings.smtp_password_set).toBe(true);
  });

  it('sends a test smtp email via POST', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);

    const result = await api.testSmtp({ to: 'a@b.co' });

    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/system/smtp/test');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body as string)).toEqual({ to: 'a@b.co' });
    expect(result.ok).toBe(true);
  });

  it('passes totp_code to login and surfaces the totp_required challenge', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ totp_required: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const api = createPortalApi(fetcher);
    const res = await api.login('e@example.test', 'pw', '123456');
    const [, init] = fetcher.mock.calls[0];
    expect(JSON.parse(init.body as string)).toEqual({
      email: 'e@example.test',
      password: 'pw',
      totp_code: '123456',
    });
    expect(res).toEqual({ totp_required: true });
  });

  it('omits totp_code when not provided', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: 'u',
          email: 'e',
          display_name: 'd',
          role: 'user',
          preferred_language: 'de',
          totp_enabled: false,
          totp_mode: 'off',
        }),
        {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        },
      ),
    );
    const api = createPortalApi(fetcher);
    await api.login('e@example.test', 'pw');
    const [, init] = fetcher.mock.calls[0];
    expect(JSON.parse(init.body as string)).toEqual({ email: 'e@example.test', password: 'pw' });
  });

  it('posts the self-service TOTP endpoints and the admin reset', async () => {
    // Each call must get its own Response instance: a fetch Response body can
    // only be read once, and this test drives the fetcher four times against
    // the same mock.
    const fetcher = vi.fn().mockImplementation(
      async () =>
        new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    const api = createPortalApi(fetcher);
    await api.totpEnroll();
    await api.totpConfirm('111222');
    await api.totpDisable('333444');
    await api.adminResetTotp('usr_1');
    const paths = fetcher.mock.calls.map((c) => c[0]);
    expect(paths).toEqual([
      '/api/portal/totp/enroll',
      '/api/portal/totp/confirm',
      '/api/portal/totp',
      '/api/admin/users/usr_1/totp/reset',
    ]);
    expect((fetcher.mock.calls[2][1] as RequestInit).method).toBe('DELETE');
    expect(JSON.parse((fetcher.mock.calls[2][1] as RequestInit).body as string)).toEqual({
      code: '333444',
    });
  });

  describe('buildQueryString', () => {
    it('skips undefined and empty values and encodes the rest', () => {
      expect(
        buildQueryString({ page: 1, limit: 25, model: undefined, status: '', q: 'a b&c' }),
      ).toBe('?page=1&limit=25&q=a+b%26c');
    });

    it('returns an empty string when no params survive', () => {
      expect(buildQueryString({ model: undefined, status: '' })).toBe('');
    });
  });
});

// The three edge-certificate text/PEM endpoints go through a dedicated
// requestText() transport (not request<T>, which expects JSON). Direct tests
// here -- as opposed to only exercising them indirectly through
// EdgeCertificatePanel.test.tsx's mocked PortalApi -- are what actually proves
// requestText() itself parses a REAL backend error body into a PortalApiError,
// since a component test that hand-throws a PortalApiError from the mock never
// touches requestText()'s parsing code at all.
describe('edge certificate text endpoints', () => {
  it('edgeCertificateBundle GETs the bundle path with credentials and returns the PEM text as-is', async () => {
    const pem = '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n';
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        new Response(pem, { status: 200, headers: { 'Content-Type': 'application/x-pem-file' } }),
      );
    const api = createPortalApi(fetcher);

    const result = await api.edgeCertificateBundle();

    expect(fetcher).toHaveBeenCalledWith('/api/system/certificates/edge/bundle', {
      credentials: 'include',
    });
    expect(result).toBe(pem);
  });

  it('edgeCertificateKey parses a genuine 409 certificate.edge_key_managed body into a PortalApiError carrying that exact code', async () => {
    // The REAL backend shape (edge_certificates.go handleSystemEdgeCertificateKey):
    // apierror.Response("certificate.edge_key_managed", "...", "") -- not a
    // hand-constructed test double.
    const fetcher = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          error: {
            code: 'certificate.edge_key_managed',
            message: 'the gateway delivers this key to its own nginx; there is no download',
          },
        }),
        { status: 409, headers: { 'Content-Type': 'application/json' } },
      ),
    );
    const api = createPortalApi(fetcher);

    await expect(api.edgeCertificateKey()).rejects.toMatchObject({
      status: 409,
      code: 'certificate.edge_key_managed',
    });
    expect(fetcher).toHaveBeenCalledWith('/api/system/certificates/edge/key', {
      credentials: 'include',
    });
  });

  it('edgeProxyConfig falls back to a generic code when the error body is not JSON', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        new Response('internal error', { status: 500, statusText: 'Internal Server Error' }),
      );
    const api = createPortalApi(fetcher);

    await expect(api.edgeProxyConfig()).rejects.toMatchObject({
      status: 500,
      code: 'request.failed',
    });
  });
});

describe('subscribeActivity', () => {
  class MockEventSource {
    static instances: MockEventSource[] = [];
    url: string;
    withCredentials: boolean;
    onerror: ((ev: Event) => void) | null = null;
    onopen: ((ev: Event) => void) | null = null;
    closed = false;
    private listeners: Record<string, Array<() => void>> = {};

    constructor(url: string, init?: EventSourceInit) {
      this.url = url;
      this.withCredentials = init?.withCredentials ?? false;
      MockEventSource.instances.push(this);
    }
    addEventListener(type: string, cb: () => void) {
      (this.listeners[type] ||= []).push(cb);
    }
    emit(type: string) {
      (this.listeners[type] ?? []).forEach((cb) => cb());
    }
    close() {
      this.closed = true;
    }
  }

  beforeEach(() => {
    MockEventSource.instances = [];
    vi.stubGlobal('EventSource', MockEventSource as unknown as typeof EventSource);
  });

  it('opens the SSE endpoint and invokes the callback on activity events', () => {
    const api = createPortalApi(vi.fn());
    const onActivity = vi.fn();

    const unsubscribe = api.subscribeActivity(onActivity);

    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/portal/usage/events');
    expect(MockEventSource.instances[0].withCredentials).toBe(true);

    MockEventSource.instances[0].emit('activity');
    expect(onActivity).toHaveBeenCalledTimes(1);

    unsubscribe();
    expect(MockEventSource.instances[0].closed).toBe(true);
  });

  it('reconnects with backoff after an error and stops after unsubscribe', () => {
    vi.useFakeTimers();
    try {
      const api = createPortalApi(vi.fn());
      const unsubscribe = api.subscribeActivity(vi.fn());

      expect(MockEventSource.instances).toHaveLength(1);
      MockEventSource.instances[0].onerror?.(new Event('error'));

      // first backoff is 1000ms -> a second connection is opened
      vi.advanceTimersByTime(1000);
      expect(MockEventSource.instances).toHaveLength(2);

      unsubscribe();
      MockEventSource.instances[1].onerror?.(new Event('error'));
      vi.advanceTimersByTime(60000);

      // no further reconnect after unsubscribe
      expect(MockEventSource.instances).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('subscribeServerPerf', () => {
  class MockEventSource {
    static instances: MockEventSource[] = [];
    url: string;
    withCredentials: boolean;
    onerror: ((ev: Event) => void) | null = null;
    onopen: ((ev: Event) => void) | null = null;
    closed = false;
    private listeners: Record<string, Array<(ev: { data: string }) => void>> = {};

    constructor(url: string, init?: EventSourceInit) {
      this.url = url;
      this.withCredentials = init?.withCredentials ?? false;
      MockEventSource.instances.push(this);
    }
    addEventListener(type: string, cb: (ev: { data: string }) => void) {
      (this.listeners[type] ||= []).push(cb);
    }
    emit(type: string, data: unknown) {
      (this.listeners[type] ?? []).forEach((cb) => cb({ data: JSON.stringify(data) }));
    }
    close() {
      this.closed = true;
    }
  }

  beforeEach(() => {
    MockEventSource.instances = [];
    vi.stubGlobal('EventSource', MockEventSource as unknown as typeof EventSource);
  });

  const point = (t: string) => ({
    t,
    cpu_util_pct: 10,
    mem_used_bytes: 1,
    mem_total_bytes: 2,
    swap_used_bytes: 0,
    swap_total_bytes: 0,
    load1: 0,
    load5: 0,
    load15: 0,
    active_requests: 0,
    queue_depth: 0,
    gpus: [],
    net: [],
  });

  it('opens the per-server SSE endpoint and dispatches snapshot + sample frames', () => {
    const api = createPortalApi(vi.fn());
    const onSnapshot = vi.fn();
    const onSample = vi.fn();
    const onStatus = vi.fn();

    const unsubscribe = api.subscribeServerPerf('srv 1', onSnapshot, onSample, onStatus);

    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/portal/servers/srv%201/perf/events');
    expect(MockEventSource.instances[0].withCredentials).toBe(true);

    MockEventSource.instances[0].onopen?.(new Event('open'));
    expect(onStatus).toHaveBeenCalledWith('open');

    const snap = [point('2026-07-19T12:00:00Z'), point('2026-07-19T12:00:05Z')];
    MockEventSource.instances[0].emit('snapshot', { points: snap });
    expect(onSnapshot).toHaveBeenCalledTimes(1);
    expect(onSnapshot.mock.calls[0][0]).toHaveLength(2);
    expect(onSnapshot.mock.calls[0][0][0].t).toBe('2026-07-19T12:00:00Z');

    const sample = point('2026-07-19T12:00:10Z');
    MockEventSource.instances[0].emit('sample', sample);
    expect(onSample).toHaveBeenCalledTimes(1);
    expect(onSample.mock.calls[0][0].t).toBe('2026-07-19T12:00:10Z');

    unsubscribe();
    expect(MockEventSource.instances[0].closed).toBe(true);
  });

  it('ignores a malformed frame without throwing', () => {
    const api = createPortalApi(vi.fn());
    const onSnapshot = vi.fn();
    const onSample = vi.fn();

    api.subscribeServerPerf('srv1', onSnapshot, onSample);

    // Feed raw invalid JSON directly to the registered listeners.
    const src = MockEventSource.instances[0] as unknown as {
      emit: (type: string, data: unknown) => void;
    };
    // emit() JSON.stringifies, so simulate a malformed payload by calling the
    // listener with a non-JSON string.
    const listeners = (
      MockEventSource.instances[0] as unknown as {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        listeners: Record<string, Array<(ev: { data: string }) => void>>;
      }
    ).listeners;
    expect(() => listeners['snapshot'].forEach((cb) => cb({ data: 'not json' }))).not.toThrow();
    expect(() => listeners['sample'].forEach((cb) => cb({ data: '{bad' }))).not.toThrow();
    expect(onSnapshot).not.toHaveBeenCalled();
    expect(onSample).not.toHaveBeenCalled();
    void src;
  });

  it('reconnects with backoff after an error and stops after unsubscribe', () => {
    vi.useFakeTimers();
    try {
      const api = createPortalApi(vi.fn());
      const unsubscribe = api.subscribeServerPerf('srv1', vi.fn(), vi.fn());

      expect(MockEventSource.instances).toHaveLength(1);
      MockEventSource.instances[0].onerror?.(new Event('error'));

      vi.advanceTimersByTime(1000);
      expect(MockEventSource.instances).toHaveLength(2);

      unsubscribe();
      MockEventSource.instances[1].onerror?.(new Event('error'));
      vi.advanceTimersByTime(60000);

      expect(MockEventSource.instances).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('subscribeLogs', () => {
  class MockEventSource {
    static instances: MockEventSource[] = [];
    url: string;
    withCredentials: boolean;
    onerror: ((ev: Event) => void) | null = null;
    onopen: ((ev: Event) => void) | null = null;
    closed = false;
    listeners: Record<string, Array<(ev: { data: string }) => void>> = {};

    constructor(url: string, init?: EventSourceInit) {
      this.url = url;
      this.withCredentials = init?.withCredentials ?? false;
      MockEventSource.instances.push(this);
    }
    addEventListener(type: string, cb: (ev: { data: string }) => void) {
      (this.listeners[type] ||= []).push(cb);
    }
    emit(type: string, data: unknown) {
      (this.listeners[type] ?? []).forEach((cb) => cb({ data: JSON.stringify(data) }));
    }
    close() {
      this.closed = true;
    }
  }

  beforeEach(() => {
    MockEventSource.instances = [];
    vi.stubGlobal('EventSource', MockEventSource as unknown as typeof EventSource);
  });

  const record = (msg: string) => ({ t: '2026-07-22T12:00:00Z', level: 'INFO', msg });

  it('opens the log SSE endpoint and dispatches snapshot + record frames', () => {
    const api = createPortalApi(vi.fn());
    const onSnapshot = vi.fn();
    const onRecord = vi.fn();
    const onStatus = vi.fn();

    const unsubscribe = api.subscribeLogs(onSnapshot, onRecord, onStatus);

    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0].url).toBe('/api/system/logs/events');
    expect(MockEventSource.instances[0].withCredentials).toBe(true);

    MockEventSource.instances[0].onopen?.(new Event('open'));
    expect(onStatus).toHaveBeenCalledWith('open');

    const snap = [record('first'), record('second')];
    MockEventSource.instances[0].emit('snapshot', { records: snap, level: 'debug' });
    expect(onSnapshot).toHaveBeenCalledTimes(1);
    expect(onSnapshot.mock.calls[0][0]).toHaveLength(2);
    expect(onSnapshot.mock.calls[0][0][0].msg).toBe('first');
    expect(onSnapshot.mock.calls[0][1]).toBe('debug');

    MockEventSource.instances[0].emit('record', record('live'));
    expect(onRecord).toHaveBeenCalledTimes(1);
    expect(onRecord.mock.calls[0][0].msg).toBe('live');

    unsubscribe();
    expect(MockEventSource.instances[0].closed).toBe(true);
  });

  it('ignores a malformed frame without throwing', () => {
    const api = createPortalApi(vi.fn());
    const onSnapshot = vi.fn();
    const onRecord = vi.fn();

    api.subscribeLogs(onSnapshot, onRecord);

    const listeners = MockEventSource.instances[0].listeners;
    expect(() => listeners['snapshot'].forEach((cb) => cb({ data: 'not json' }))).not.toThrow();
    expect(() => listeners['record'].forEach((cb) => cb({ data: '{bad' }))).not.toThrow();
    expect(onSnapshot).not.toHaveBeenCalled();
    expect(onRecord).not.toHaveBeenCalled();
  });

  it('reconnects with backoff after an error and stops after unsubscribe', () => {
    vi.useFakeTimers();
    try {
      const api = createPortalApi(vi.fn());
      const unsubscribe = api.subscribeLogs(vi.fn(), vi.fn());

      expect(MockEventSource.instances).toHaveLength(1);
      MockEventSource.instances[0].onerror?.(new Event('error'));

      vi.advanceTimersByTime(1000);
      expect(MockEventSource.instances).toHaveLength(2);

      unsubscribe();
      MockEventSource.instances[1].onerror?.(new Event('error'));
      vi.advanceTimersByTime(60000);

      expect(MockEventSource.instances).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });
});
