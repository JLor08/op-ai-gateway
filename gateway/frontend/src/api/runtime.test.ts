// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import {
  createPortalApi,
  PortalApiError,
  type CoResidency,
  type PutRuntimeSpecRequest,
  type RuntimeSpec,
  type RuntimeStatus,
} from '../api';
import { formatPortalError } from '../components/shared/format';
import { messages } from '../i18n';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

// The co-residency document is `{mode, pairs:[{a,b,verdict}]}`: the per-server
// mode and every EXPLICIT pair with its own verdict (Go CoResidencyDTO). A pair
// with no entry is not listed at all; the mode says what that means.
describe('runtimeApi co-residency', () => {
  const document: CoResidency = {
    mode: 'opt_out',
    pairs: [
      { a: 'map_1', b: 'map_2', verdict: 'forbid' },
      { a: 'map_1', b: 'map_3', verdict: 'allow' },
    ],
  };

  it('GETs the mode and the verdict-carrying pairs verbatim, application id URL-encoded', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(document));
    const api = createPortalApi(fetcher);

    const got = await api.runtimeCoresidency('app/1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/applications/app%2F1/runtime/coresidency', {
      headers: {},
      credentials: 'include',
    });
    expect(got).toEqual(document);
    expect(got.mode).toBe('opt_out');
    expect(got.pairs[0].verdict).toBe('forbid');
  });

  it('PUTs the FULL replacement set together with the mode, as {mode, pairs:[{a,b,verdict}]}', async () => {
    const stored: CoResidency = { mode: 'opt_in', pairs: document.pairs };
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(stored));
    const api = createPortalApi(fetcher);

    const resp = await api.putRuntimeCoresidency('app_1', stored);

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/applications/app_1/runtime/coresidency');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    // Verdicts survive the serialisation: a forbid must never be folded into an
    // allow on the way out, and the mode rides every write (an omitted mode is
    // "leave it alone" server-side, but the editor always states it).
    expect(JSON.parse(init.body)).toEqual({
      mode: 'opt_in',
      pairs: [
        { a: 'map_1', b: 'map_2', verdict: 'forbid' },
        { a: 'map_1', b: 'map_3', verdict: 'allow' },
      ],
    });
    expect(resp).toEqual(stored);
  });

  it('PUTs an empty set as an explicit [] (never null), still with the mode', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ mode: 'opt_out', pairs: [] }));
    const api = createPortalApi(fetcher);

    await api.putRuntimeCoresidency('app_1', { mode: 'opt_out', pairs: [] });

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({ mode: 'opt_out', pairs: [] });
  });
});

// The gateway answers a mode outside opt_in/opt_out with 400
// runtime_coresidency.mode_invalid; the portal renders a localized label for it
// rather than the server's bare English text.
describe('runtime_coresidency.mode_invalid', () => {
  it('maps to its own localized label, in both locales', () => {
    const err = new PortalApiError(400, 'runtime_coresidency.mode_invalid', 'raw server text');
    expect(formatPortalError(err, messages.de)).toBe(
      `runtime_coresidency.mode_invalid: ${messages.de.errorRuntimeCoresidencyModeInvalid}`,
    );
    expect(formatPortalError(err, messages.en)).toBe(
      `runtime_coresidency.mode_invalid: ${messages.en.errorRuntimeCoresidencyModeInvalid}`,
    );
    expect(messages.de.errorRuntimeCoresidencyModeInvalid).not.toBe(
      messages.de.errorRuntimeCoresidencyPairInvalid,
    );
  });
});

// The spec the editor groups by carries the backend's DERIVED role
// (RuntimeSpecDTO.role, routing.DeriveRole): a read-only echo, so the PUT
// request type must not accept it back.
describe('runtimeApi runtime spec role', () => {
  it('reads the derived role off a GET', async () => {
    const spec = { configured: true, mapping_id: 'map_1', role: 'asr' } as RuntimeSpec;
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(spec));
    const api = createPortalApi(fetcher);

    const got = await api.runtimeSpec('map_1');

    expect(got.role).toBe('asr');
  });

  it('keeps role out of the PUT request shape', () => {
    // Compile-time: a request carrying `role` is an excess-property error.
    const body: Parameters<ReturnType<typeof createPortalApi>['putRuntimeSpec']>[1] = {
      enabled: true,
      binary: 'x',
      args: [],
      env: {},
      work_dir: '',
      listen_port: 0,
      health_path: '',
      health_timeout_seconds: 0,
      startup_timeout_seconds: 0,
      idle_timeout_seconds: 0,
      admission_wait_timeout_seconds: 0,
      pinned: false,
      admin_state: '',
      set_visible_devices: false,
      visible_devices_mode: 'env',
      gpus: [],
      ram_estimate_mb: 0,
      replicable: false,
      min_instances: 0,
      max_instances: 0,
      instance_parallelism: 0,
      api_token_mode: 'app',
      api_token_header_source: 'app',
      api_token_header: '',
      api_flavors: [],
      responses_mode: 'passthrough',
      messages_mode: 'passthrough',
      type: '',
      metrics_path: '',
      context_probe_path: '',
      // @ts-expect-error role is a read-only echo, not part of the request
      role: 'asr',
    };
    expect(body).toBeDefined();
  });
});

// The instance settings (#340): four operator-owned fields the PUT carries like
// every other field of the full document, and two read-only figures beside
// them -- the agent's measured startup and the idle timeout the gateway pushes
// for the stored spec -- that only a GET carries.
describe('runtimeApi runtime spec instance settings', () => {
  it('reads the four settings and the two read-only figures off a GET', async () => {
    const spec = {
      configured: true,
      mapping_id: 'map_1',
      replicable: true,
      min_instances: 1,
      max_instances: 0,
      instance_parallelism: 2,
      startup_measured_ms: 30000,
      effective_idle_timeout_seconds: 150,
    } as RuntimeSpec;
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(spec));
    const api = createPortalApi(fetcher);

    const got = await api.runtimeSpec('map_1');

    expect(got).toMatchObject({
      replicable: true,
      min_instances: 1,
      max_instances: 0,
      instance_parallelism: 2,
      startup_measured_ms: 30000,
      effective_idle_timeout_seconds: 150,
    });
  });

  it('PUTs the four settings verbatim, an unlimited maximum as 0', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ configured: true }));
    const api = createPortalApi(fetcher);
    const body: Parameters<ReturnType<typeof createPortalApi>['putRuntimeSpec']>[1] = {
      enabled: true,
      binary: 'x',
      args: [],
      env: {},
      work_dir: '',
      listen_port: 0,
      health_path: '',
      health_timeout_seconds: 0,
      startup_timeout_seconds: 0,
      idle_timeout_seconds: 0,
      admission_wait_timeout_seconds: 0,
      pinned: false,
      admin_state: '',
      set_visible_devices: false,
      visible_devices_mode: 'env',
      gpus: [],
      ram_estimate_mb: 0,
      replicable: true,
      min_instances: 1,
      max_instances: 0,
      instance_parallelism: 2,
      api_token_mode: 'app',
      api_token_header_source: 'app',
      api_token_header: '',
      api_flavors: [],
      responses_mode: 'passthrough',
      messages_mode: 'passthrough',
      type: '',
      metrics_path: '',
      context_probe_path: '',
    };

    await api.putRuntimeSpec('map_1', body);

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toMatchObject({
      replicable: true,
      min_instances: 1,
      max_instances: 0,
      instance_parallelism: 2,
    });
  });

  it('keeps the read-only figures and the real-time warning out of the PUT request shape', () => {
    // Compile-time: each is an excess-property error on the request.
    const base: Parameters<ReturnType<typeof createPortalApi>['putRuntimeSpec']>[1] = {
      enabled: true,
      binary: 'x',
      args: [],
      env: {},
      work_dir: '',
      listen_port: 0,
      health_path: '',
      health_timeout_seconds: 0,
      startup_timeout_seconds: 0,
      idle_timeout_seconds: 0,
      admission_wait_timeout_seconds: 0,
      pinned: false,
      admin_state: '',
      set_visible_devices: false,
      visible_devices_mode: 'env',
      gpus: [],
      ram_estimate_mb: 0,
      replicable: false,
      min_instances: 0,
      max_instances: 0,
      instance_parallelism: 0,
      api_token_mode: 'app',
      api_token_header_source: 'app',
      api_token_header: '',
      api_flavors: [],
      responses_mode: 'passthrough',
      messages_mode: 'passthrough',
      type: '',
      metrics_path: '',
      context_probe_path: '',
    };
    const withStartup: typeof base = {
      ...base,
      // @ts-expect-error startup_measured_ms is agent-owned, not part of the request
      startup_measured_ms: 30000,
    };
    const withEffective: typeof base = {
      ...base,
      // @ts-expect-error effective_idle_timeout_seconds is derived, not part of the request
      effective_idle_timeout_seconds: 150,
    };
    const withRealtimeWarning: typeof base = {
      ...base,
      // @ts-expect-error instances_min_exceeds_realtime is derived, not part of the request
      instances_min_exceeds_realtime: true,
    };
    expect(withStartup).toBeDefined();
    expect(withEffective).toBeDefined();
    expect(withRealtimeWarning).toBeDefined();
  });
});

// The two refusals of the instance settings: a min above the max (or a count
// above the agent's cap of 64), and several instances on one fixed port. Each
// renders its own localized label, never the server's English text.
describe('runtime_spec instance-settings refusals', () => {
  it.each([
    ['runtime_spec.instances_invalid', 'errorRuntimeSpecInstancesInvalid'],
    ['runtime_spec.instances_need_ephemeral_port', 'errorRuntimeSpecInstancesNeedEphemeralPort'],
  ] as const)('maps %s to %s, in both locales', (code, key) => {
    const err = new PortalApiError(400, code, 'raw server text');
    expect(formatPortalError(err, messages.de)).toBe(`${code}: ${messages.de[key]}`);
    expect(formatPortalError(err, messages.en)).toBe(`${code}: ${messages.en[key]}`);
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });
});

// The live-status Play and Stop: transient actions on one instance of a
// mapping's managed model. Neither writes the spec; both answer the instance
// they addressed and the agent's state for it.
describe('runtimeApi manual start and stop', () => {
  it('POSTs a start without a body, mapping id URL-encoded, and returns the answer', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(jsonResponse({ instance: 2, state: 'starting' }, 202));
    const api = createPortalApi(fetcher);

    const got = await api.startRuntime('map/1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/mappings/map%2F1/runtime/start');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(got).toEqual({ instance: 2, state: 'starting' });
  });

  it('POSTs a stop with the instance index as its body and returns the answer', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ instance: 0, state: 'stopping' }));
    const api = createPortalApi(fetcher);

    const got = await api.stopRuntime('map_1', 0);

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/mappings/map_1/runtime/stop');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    // Instance 0 is sent as a number, not dropped as falsy: the gateway
    // refuses a body without an index (runtime.instance_invalid).
    expect(JSON.parse(init.body)).toEqual({ instance: 0 });
    expect(got).toEqual({ instance: 0, state: 'stopping' });
  });

  it('sends stop_members only when the members are to be stopped too', async () => {
    const fetcher = vi
      .fn()
      .mockImplementation(async () => jsonResponse({ instance: 0, state: 'stopping' }));
    const api = createPortalApi(fetcher);

    await api.stopRuntime('map_1', 0, true);
    await api.stopRuntime('map_1', 0, false);

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({ instance: 0, stop_members: true });
    expect(JSON.parse(fetcher.mock.calls[1][1].body)).toEqual({ instance: 0 });
  });

  it('carries the missing feature of a runtime.agent_feature_missing refusal', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          error: {
            code: 'runtime.agent_feature_missing',
            message: 'the agent does not declare runtime_stop',
            feature: 'runtime_stop',
          },
        },
        409,
      ),
    );
    const api = createPortalApi(fetcher);

    const err = await api.stopRuntime('map_1', 1).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(PortalApiError);
    expect((err as PortalApiError).status).toBe(409);
    expect((err as PortalApiError).code).toBe('runtime.agent_feature_missing');
    expect((err as PortalApiError).feature).toBe('runtime_stop');
  });
});

// The RPC cluster additions: the cluster-candidates read and the request
// shape of a cluster head save.
describe('runtimeApi RPC cluster', () => {
  it('GETs the cluster candidates for a mode, mapping id URL-encoded', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: [{ server_id: 's1' }] }));
    const got = await createPortalApi(fetcher).clusterCandidates('map/1', 'manual');
    expect(fetcher.mock.calls[0][0]).toBe(
      '/api/portal/mappings/map%2F1/runtime/cluster-candidates?mode=manual',
    );
    expect(got.data[0].server_id).toBe('s1');
  });

  it('carries the cluster fields on a PUT and excludes the read-only totals', () => {
    const body: PutRuntimeSpecRequest = {
      ...({} as PutRuntimeSpecRequest),
      device_backend: 'auto',
      cluster_enabled: true,
      member_mode: 'automatic',
      cluster_network: 'mesh',
      rpc_transport: 'relay',
      lan_exposure_ack: false,
      cluster_members: [],
    };
    expect(body.cluster_enabled).toBe(true);
    const bad: PutRuntimeSpecRequest = {
      ...body,
      // @ts-expect-error cluster_vram_total_mb is derived, not part of the request
      cluster_vram_total_mb: 1,
    };
    expect(bad).toBeDefined();
  });

  it('types the RPC telemetry on a status row', () => {
    const row: RuntimeStatus = {
      ...({} as RuntimeStatus),
      kind: 'rpc_server',
      rpc: {
        held_by: 'rspec_1',
        held_since: '2026-01-01T00:00:00Z',
        reserved_until: '',
        protocol: '8.0.1',
        device_count: 2,
        bind_mismatch: false,
        breaches: [{ peer: '100.64.0.9', at: '2026-01-01T00:00:00Z' }],
        cache_mb: 0,
        stalled: false,
      },
      cluster: { launched: ['100.64.0.5:47001'], version: 'b1234' },
      admission_blocked_by: 'rpcm_x',
      last_error: {
        code: 'runtime.bind_mismatch',
        message: 'm',
        at: '',
        exit_code: 0,
        failures: 1,
      },
    };
    expect(row.rpc?.breaches?.[0].peer).toBe('100.64.0.9');
  });
});
