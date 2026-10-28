// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi } from '../api';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

// The Voice Chat ticket POST (issue #312): the path, method, CSRF header and body must match the
// gateway's handlePortalVoiceChatTicket exactly (voice_chat_ticket_endpoint.go).
describe('voiceChatApi createVoiceChatTicket', () => {
  it('POSTs the chat and the three models, with the CSRF header, and returns the ticket', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ticket: 'tkt_voice' }));

    const result = await createPortalApi(fetcher).createVoiceChatTicket({
      chatId: 'chat_1',
      asrModel: 'org/asr one',
      llmModel: 'chat-model',
      ttsModel: 'speech-model',
      voice: 'alba',
    });

    expect(result).toEqual({ ticket: 'tkt_voice' });
    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/voice-chat-ticket');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.headers['Content-Type']).toBe('application/json');
    // The names travel in the body (a gateway name may hold '/'), never in the URL.
    expect(JSON.parse(init.body)).toEqual({
      chat_id: 'chat_1',
      asr_model: 'org/asr one',
      llm_model: 'chat-model',
      tts_model: 'speech-model',
      voice: 'alba',
    });
  });

  it('sends the voice as an empty string when none is chosen (the speech model default)', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ticket: 'tkt_voice' }));

    await createPortalApi(fetcher).createVoiceChatTicket({
      chatId: 'chat_1',
      asrModel: 'asr',
      llmModel: 'llm',
      ttsModel: 'tts',
      voice: '',
    });

    expect(JSON.parse(fetcher.mock.calls[0][1].body).voice).toBe('');
  });

  // The gateway's refusals of a Voice Chat ticket: a leg it cannot use (400), models it cannot
  // hold together (409), a chat that is not the caller's (404), a mint that failed (500). The
  // portal tells the user why from the code, so the client hands it on exactly as sent.
  it.each([
    [400, 'portal.voice_chat_asr_unavailable'],
    [400, 'portal.voice_chat_llm_unavailable'],
    [400, 'portal.voice_chat_tts_unavailable'],
    [400, 'portal.voice_chat_audio_settings_invalid'],
    [409, 'portal.voice_chat_coload_failed'],
    [404, 'portal.chat_not_found'],
    [500, 'portal.voice_chat_ticket_failed'],
  ])(
    'surfaces a refused ticket (%i %s) as a PortalApiError carrying the code',
    async (status, code) => {
      const fetcher = vi
        .fn()
        .mockResolvedValue(jsonResponse({ error: { code, message: 'refused' } }, status));

      await expect(
        createPortalApi(fetcher).createVoiceChatTicket({
          chatId: 'chat_1',
          asrModel: 'asr',
          llmModel: 'llm',
          ttsModel: 'tts',
          voice: '',
        }),
      ).rejects.toMatchObject({ status, code });
    },
  );
});
