// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { PortalApiError, request } from './transport';

function respond(status: number, body: string, contentType: string) {
  return async () => new Response(body, { status, headers: { 'Content-Type': contentType } });
}

describe('request error codes', () => {
  it('turns an HTML 413 from a proxy into request.body_too_large', async () => {
    const fetcher = respond(
      413,
      '<html><body><center><h1>413 Request Entity Too Large</h1></center></body></html>',
      'text/html',
    );
    const err = await request(fetcher, '/api/portal/chats/c1', { method: 'PUT', body: {} }).catch(
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(PortalApiError);
    expect(err).toMatchObject({ status: 413, code: 'request.body_too_large' });
  });

  // The code differs from the 413 fallback on purpose: with the same string,
  // a request() that preferred the status over the JSON body would pass too.
  it('keeps the JSON error code of a 413 over the status fallback', async () => {
    const fetcher = respond(
      413,
      '{"error":{"code":"some.specific_code","message":"request body too large"}}',
      'application/json',
    );
    const err = await request(fetcher, '/api/portal/chats/c1', { method: 'PUT', body: {} }).catch(
      (e: unknown) => e,
    );
    expect(err).toMatchObject({ status: 413, code: 'some.specific_code' });
  });

  it('still reports request.failed for any other error without a JSON body', async () => {
    const fetcher = respond(502, '<html><body>Bad Gateway</body></html>', 'text/html');
    const err = await request(fetcher, '/api/portal/chats', {}).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 502, code: 'request.failed' });
  });
});
