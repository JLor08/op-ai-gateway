// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi } from '../api';
import { PortalApiError } from './transport';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

// The two mutations of the audio answer lifecycle (#281): the paths, methods and body must
// match the gateway's routes exactly (portal_chat_audio_endpoints.go).
describe('chatApi audio lifecycle mutations', () => {
  it('deleteMessageAudio DELETEs the answer audio route with the CSRF header and no body', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const result = await api.deleteMessageAudio('chat_1', 'msg_9');

    expect(result).toEqual({ ok: true });
    expect(fetcher).toHaveBeenCalledTimes(1);
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/chats/chat_1/messages/msg_9/audio');
    expect(init.method).toBe('DELETE');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
  });

  it('deleteMessageAudio escapes both ids into the path', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.deleteMessageAudio('chat/1', 'msg 9?');

    expect(fetcher.mock.calls[0][0]).toBe('/api/portal/chats/chat%2F1/messages/msg%209%3F/audio');
  });

  it('pruneChatAudio POSTs {keep_last} to the prune route with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const result = await api.pruneChatAudio('chat_1', 3);

    expect(result).toEqual({ ok: true });
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/chats/chat_1/audio/prune');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(init.body as string)).toEqual({ keep_last: 3 });
  });

  // 0 is the "remove all stored audio" target and a real count, not "unset": the body must
  // carry it (the gateway answers a missing keep_last with 400, never with a prune of nothing).
  it('pruneChatAudio sends an explicit keep_last of 0', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.pruneChatAudio('chat_1', 0);

    expect(JSON.parse(fetcher.mock.calls[0][1].body as string)).toEqual({ keep_last: 0 });
  });

  it('pruneChatAudio escapes the chat id into the path', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.pruneChatAudio('chat/1', 2);

    expect(fetcher.mock.calls[0][0]).toBe('/api/portal/chats/chat%2F1/audio/prune');
  });

  // The 409 while a run is live and the 404 of a foreign or unknown chat reach the caller as
  // the transport's own error, with the gateway's stable code, so the store can name them.
  it.each([
    ['deleteMessageAudio', 409, 'portal.chat_run_active'],
    ['pruneChatAudio', 409, 'portal.chat_run_active'],
    ['deleteMessageAudio', 404, 'portal.chat_not_found'],
    ['pruneChatAudio', 404, 'portal.chat_not_found'],
  ] as const)('%s surfaces a %i as a PortalApiError (%s)', async (method, status, code) => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(jsonResponse({ error: { code, message: 'refused' } }, status));
    const api = createPortalApi(fetcher);

    const call = method === 'deleteMessageAudio' ? api[method]('c', 'm') : api[method]('c', 1);

    await expect(call).rejects.toMatchObject({ status, code });
    await expect(call).rejects.toBeInstanceOf(PortalApiError);
  });
});
