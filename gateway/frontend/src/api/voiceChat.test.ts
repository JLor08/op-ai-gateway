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

// The Voice Chat re-synthesis POST (issue #324): one section of an assistant answer spoken again
// through the speech model, without a WebSocket. The path, method, CSRF header and body must match
// the gateway's handlePortalVoiceChatSynth, and the SUCCESS body is raw audio bytes, not JSON.
describe('voiceChatApi synthVoiceSection', () => {
  // A Response whose body carries real bytes, as the gateway's audio/wav answer does.
  function audioResponse(bytes: Uint8Array<ArrayBuffer>, status = 200) {
    return new Response(bytes, { status, headers: { 'Content-Type': 'audio/wav' } });
  }

  it('POSTs the section text and speech settings, with the CSRF header, and returns the audio Blob', async () => {
    const wav = new Uint8Array([0x52, 0x49, 0x46, 0x46, 0x00, 0x01, 0xff, 0xfe]);
    const fetcher = vi.fn().mockResolvedValue(audioResponse(wav));

    const result = await createPortalApi(fetcher).synthVoiceSection({
      text: 'Hello there.',
      ttsModel: 'org/speech one',
      voice: 'alba',
      language: 'en',
    });

    // The body is read as a Blob (not JSON/text). Check its shape structurally rather than
    // with `instanceof Blob`, which is unreliable across the Node/undici realms CI exercises:
    // response.blob() can hand back a Blob from a different realm than the test's global.
    expect(result.type).toBe('audio/wav');
    expect(result.size).toBe(wav.length);
    // The bytes round-trip untouched (the body was read as a Blob, not decoded as JSON or text).
    expect(new Uint8Array(await result.arrayBuffer())).toEqual(wav);
    const [path, init] = fetcher.mock.calls[0];
    expect(path).toBe('/api/portal/voice-chat-synth');
    expect(init.method).toBe('POST');
    expect(init.credentials).toBe('include');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.headers['Content-Type']).toBe('application/json');
    // The model name travels in the body (a gateway name may hold '/'), never in the URL.
    expect(JSON.parse(init.body)).toEqual({
      text: 'Hello there.',
      tts_model: 'org/speech one',
      voice: 'alba',
      language: 'en',
    });
  });

  it('omits the language from the body when none is given (the speech model keeps its own default)', async () => {
    const fetcher = vi.fn().mockResolvedValue(audioResponse(new Uint8Array([1, 2, 3])));

    await createPortalApi(fetcher).synthVoiceSection({ text: 'Hi.', ttsModel: 'tts', voice: '' });

    const body = JSON.parse(fetcher.mock.calls[0][1].body);
    expect(body).toEqual({ text: 'Hi.', tts_model: 'tts', voice: '' });
    expect('language' in body).toBe(false);
  });

  // The gateway's refusals of a synthesis: a blank text or model (400 speech.*), a speech model
  // the portal cannot use (400), a synthesis that failed. The client hands the code on as sent.
  it.each([
    [400, 'speech.input_required'],
    [400, 'speech.model_required'],
    [400, 'portal.voice_chat_tts_unavailable'],
    [502, 'speech.upstream_error'],
  ])(
    'surfaces a refused synthesis (%i %s) as a PortalApiError carrying the code',
    async (status, code) => {
      const fetcher = vi
        .fn()
        .mockResolvedValue(jsonResponse({ error: { code, message: 'refused' } }, status));

      const err = await createPortalApi(fetcher)
        .synthVoiceSection({ text: 'Hi.', ttsModel: 'tts', voice: '' })
        .catch((e: unknown) => e);

      expect(err).toBeInstanceOf(PortalApiError);
      expect(err).toMatchObject({ status, code, message: 'refused' });
    },
  );

  // A proxy in front of the gateway answers with an HTML page, not the JSON error body: the
  // binary transport names the failure the way the JSON one does.
  it.each([
    [502, 'request.failed'],
    [413, 'request.body_too_large'],
  ])(
    'falls back to a generic code (%i %s) when a refusal has no JSON error body',
    async (status, code) => {
      const fetcher = vi.fn().mockResolvedValue(
        new Response('<html>proxy page</html>', {
          status,
          headers: { 'Content-Type': 'text/html' },
        }),
      );

      await expect(
        createPortalApi(fetcher).synthVoiceSection({ text: 'Hi.', ttsModel: 'tts', voice: '' }),
      ).rejects.toMatchObject({ status, code });
    },
  );
});
