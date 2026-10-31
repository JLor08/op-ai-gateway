// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { PortalApiError, request, requestBinary } from './transport';

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

describe('requestBinary', () => {
  it('returns the success body as a Blob without reading it as JSON', async () => {
    const bytes = new Uint8Array([0x00, 0xff, 0x7b, 0x80, 0x52]);
    const seen: { path: string; init: RequestInit | undefined }[] = [];
    const fetcher = async (path: RequestInfo | URL, init?: RequestInit) => {
      seen.push({ path: String(path), init });
      return new Response(bytes, { status: 200, headers: { 'Content-Type': 'audio/wav' } });
    };

    const blob = await requestBinary(fetcher, '/api/portal/clip');

    expect(blob.type).toBe('audio/wav');
    expect(new Uint8Array(await blob.arrayBuffer())).toEqual(bytes);
    // A plain GET: the session cookie, but neither the CSRF header nor a body.
    expect(seen[0].path).toBe('/api/portal/clip');
    expect(seen[0].init?.credentials).toBe('include');
    expect(seen[0].init?.headers).toEqual({});
    expect(seen[0].init?.body).toBeUndefined();
  });

  it('parses a JSON error body into a PortalApiError, like request', async () => {
    const fetcher = respond(
      400,
      '{"error":{"code":"speech.input_required","message":"input is required"}}',
      'application/json',
    );
    const err = await requestBinary(fetcher, '/api/portal/clip', {
      method: 'POST',
      body: {},
    }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PortalApiError);
    expect(err).toMatchObject({
      status: 400,
      code: 'speech.input_required',
      message: 'input is required',
    });
  });
});
