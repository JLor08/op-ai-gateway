// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi, PortalApiError, type CoResidency, type RuntimeSpec } from '../api';
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
