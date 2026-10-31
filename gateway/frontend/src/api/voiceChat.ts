// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Voice Chat domain module (issue #312): the HTTP calls of a Voice Chat session -- the ticket POST
// and, since #324, the section re-synthesis POST. The conversation itself rides a WebSocket
// (components/chat/voiceSession.ts); the ticket is the authenticated, CSRF-protected step before
// it, the twin of createLiveTranscriptionTicket.

import { type Fetcher, request, requestBinary } from './transport';

// What a Voice Chat ticket is minted for: the chat the spoken turns belong to, the three gateway
// models of the loop, by the name the portal lists them under -- the live-transcription model
// (ears), the chat model (brain) and the streaming speech model (voice) -- and the voice the
// speech model answers in ('' is the model's own default voice).
export type VoiceChatTicketArgs = {
  chatId: string;
  asrModel: string;
  llmModel: string;
  ttsModel: string;
  voice: string;
};

export function voiceChatApi(fetcher: Fetcher) {
  return {
    // Mint the single-use ticket a Voice Chat WebSocket upgrade presents. A browser WebSocket
    // cannot send the portal's X-OP-CSRF header, so the upgrade cannot be protected the way
    // every other portal mutation is: this POST is (the transport adds the header and the
    // session cookie), and the ticket it returns -- short-lived, single-use, opaque -- is what
    // the upgrade carries instead. The gateway resolves the three models and holds them in memory
    // together (cold-starting them if need be, so this request can take a long while) before it
    // answers; the PCM the browser then streams is fixed by the capture, not chosen here. A
    // refusal -- an unusable model (400), models that cannot be held together (409
    // portal.voice_chat_coload_failed), an unknown chat (404) -- is a PortalApiError carrying the
    // gateway's code. The voice rides in the body like the models, and the ticket's speech
    // settings take it (it wins over the chat's stored settings.audio voice, which a text thread
    // has none of).
    createVoiceChatTicket: (args: VoiceChatTicketArgs) =>
      request<{ ticket: string }>(fetcher, '/api/portal/voice-chat-ticket', {
        method: 'POST',
        body: {
          chat_id: args.chatId,
          asr_model: args.asrModel,
          llm_model: args.llmModel,
          tts_model: args.ttsModel,
          voice: args.voice,
        },
      }),

    // Speak one section of an assistant answer again (issue #324), without a WebSocket and
    // without touching the chat: POST the section's text and the speech settings, and get back
    // the finished audio as a Blob (audio/wav). The success body is raw bytes, not JSON, so this
    // rides the binary transport; a refusal -- a blank text or model (400 speech.input_required,
    // speech.model_required), a speech model the portal cannot use (400
    // portal.voice_chat_tts_unavailable), a synthesis that failed -- is a PortalApiError
    // carrying the gateway's code. The model and voice travel in the body like the ticket's ('' is
    // the model's own default voice); a language is sent only when given, so the speech model
    // otherwise stays on its own default.
    synthVoiceSection: (args: {
      text: string;
      ttsModel: string;
      voice: string;
      language?: string;
    }) =>
      requestBinary(fetcher, '/api/portal/voice-chat-synth', {
        method: 'POST',
        body: {
          text: args.text,
          tts_model: args.ttsModel,
          voice: args.voice,
          language: args.language,
        },
      }),
  };
}
