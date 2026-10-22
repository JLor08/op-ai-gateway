// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';

// Persistent chat playground documents. `content` is opaque to the backend
// (stored encrypted); the frontend owns its shape (ChatContentDoc below). A
// ChatSummary is the list-view row (no content); a Chat is the full document.
export type ChatSummary = {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
};

export type Chat = ChatSummary & { content: unknown };

// The chat listing. The wire body also carries the backend's pre-seal content
// cap (max_content_bytes), which this client deliberately does not read: images
// are stored out of the transcript, so the document size no longer says how many
// more fit, and text overflow is the server's 400 portal.chat_too_large.
export type ChatListResponse = {
  data: ChatSummary[];
};

// The image-generation knobs of an "image" thread, as sent on the wire and
// persisted inside `content.settings.image`. Every field is OPTIONAL: an unset
// knob is omitted so the upstream (sd-server) keeps its own default, and the
// whole object is omitted for a text thread (mirroring `kind`), so a text chat's
// persisted settings stay byte-identical. Only the fields sd-server's OpenAI
// /v1/images/generations endpoint honors are here — size, n, output_format and
// output_compression — see the backend's portal.ChatImageSettings.
export type ChatImageSettings = {
  size?: string;
  n?: number;
  output_format?: string;
  output_compression?: number;
  // The advanced sd-server knobs (Phase 1b). The backend folds the set ones into
  // a <sd_cpp_extra_args> block in the prompt (not top-level fields upstream);
  // the numerics are sent as JSON numbers, omitted when the composer left them
  // blank. See the backend's portal.ChatImageSettings.
  negative_prompt?: string;
  sampler?: string;
  scheduler?: string;
  steps?: number;
  cfg?: number;
  seed?: number;
  clip_skip?: number;
  // The EDIT knob (how far an edit departs from its source image): omitted =
  // sd-server's own default (0.75). Rides the same <sd_cpp_extra_args> block;
  // sd-server ignores it for a generation.
  strength?: number;
};

// The text-to-speech knobs of a "speech" thread, as sent on the wire and persisted
// inside `content.settings.audio`. Like ChatImageSettings every field is OPTIONAL: an
// unset knob is omitted so the model keeps its own default, and the whole object is
// omitted for a text or image thread (mirroring `kind`), so their persisted settings
// stay byte-identical. The reply is always WAV (the gateway serves only wav), so there
// is no format knob. See the backend's portal.ChatAudioSettings.
export type ChatAudioSettings = {
  // The voice name, picked from the voices the gateway has cached for the model or
  // typed as free text (the composer's field is a combobox that still takes any name);
  // omitted = the model's default voice.
  voice?: string;
  // The speaking-rate multiplier, passed through as given (the model's own range
  // applies); omitted = the model's default.
  speed?: number;
  // The advanced extra-JSON surface: top-level request fields merged verbatim into the
  // /v1/audio/speech body (the OpenAI `instructions` field and any audio.cpp-native
  // knob). The values keep the JSON type the user wrote. The backend refuses the keys
  // the run owns (model / input / stream / stream_format / response_format).
  passthrough?: Record<string, unknown>;
  // The thread's choice to hear a streaming-capable model's audio while it is generated
  // (ADR-068). A CHOICE, not a request: the gateway streams only where its own gate also
  // holds (the model streams and its output rate is known) and otherwise answers the
  // buffered WAV whatever this says. Omitted = the default, which streams where it can;
  // only an explicit `false` opts the thread out, so an unset thread persists nothing.
  stream?: boolean;
  // How many of the thread's most recent spoken answers keep their stored audio (#281).
  // Omitted = keep every answer (the default, and what every thread from before this knob
  // does); `0` = do not store the audio at all (the answer is still played as it is
  // generated, the text stays); `N >= 1` = keep the last N answers' audio, and the audio of
  // older answers is pruned when a new answer is committed. Only 0 and a positive integer
  // are ever written, so an unset thread persists nothing.
  keep_last_audio?: number;
};

// The speech-to-text knobs of a "transcription" thread, as sent on the wire and persisted
// inside `content.settings.transcription`. Like ChatAudioSettings every field is OPTIONAL:
// an unset knob is omitted so the model keeps its own default, and the whole object is
// omitted for a text, image or speech thread (mirroring `kind`), so their persisted
// settings stay byte-identical. The reply is always the transcript as text (the gateway
// asks the endpoint for JSON), so there is no response format knob. See the backend's
// portal.ChatTranscriptionSettings.
export type ChatTranscriptionSettings = {
  // The ISO-639-1 code of the spoken language (e.g. "de"); omitted = the model detects it.
  language?: string;
  // Optional text that steers the transcription's style or primes it with names and
  // jargon; omitted = none.
  prompt?: string;
  // The sampling temperature, passed through as given (the model's own range applies).
  // A number INCLUDING an explicit 0 (a real, deterministic setting, not "unset");
  // omitted = the model's default.
  temperature?: number;
  // The advanced extra-JSON surface: extra request fields merged into the
  // /v1/audio/transcriptions form verbatim (the OpenAI timestamp granularities and any
  // audio.cpp-native knob). The values keep the JSON type the user wrote (the backend
  // writes a string as itself and any other value as its compact JSON). The backend
  // refuses the keys the run owns (model / file / response_format / stream).
  passthrough?: Record<string, unknown>;
};

// The per-chat settings the frontend persists inside `content`.
export type ChatSettings = {
  model: string;
  system_prompt: string;
  temperature: number;
  max_tokens: number;
  run_as_token_id: string;
  // Per-chat server override (the chat analog of PortalToken.server_override):
  // forces every request the background run for this chat generates onto one
  // specific AI-server the chat owner manages. "" = no override. Self-healed
  // server-side on every run (PrepareChatRun), same as the token's.
  server_override: string;
  server_override_force_unreachable: boolean;
  // The thread's pinned kind: "" (text — the default and every pre-existing
  // chat), "image", "speech" or "transcription". Established by the FIRST send and then
  // FORCED by the backend on every later send (PrepareChatRun), because the composer's
  // affordances follow the THREAD, not the currently-picked model.
  //
  // Typed as a plain string rather than a closed union on purpose: a kind this
  // build does not recognise must round-trip through load/save untouched. The
  // backend full-replaces this whole blob on every PUT with no merge, so a
  // field the frontend drops is a field the next autosave silently erases.
  //
  // OPTIONAL, and OMITTED (not written as "") for a text thread: the backend
  // declares Kind last with `json:"kind,omitempty"` precisely so an existing
  // text chat's persisted settings stay byte-identical, and it has a test
  // asserting the key never appears for one. Writing `"kind":""` back on
  // every save would give that property away from the client side. See
  // kindSetting() in chatDoc.ts, which is how both writers honour it.
  kind?: string;
  // The image-generation knobs, present only on an "image" thread (omitted for a
  // text thread so its settings stay byte-identical, exactly like `kind`). The
  // store holds a concrete ImageSettings (chatDoc.ts) and both writers go
  // through imageSetting() so neither can reintroduce the key for a text thread.
  image?: ChatImageSettings;
  // The text-to-speech knobs, present only on a "speech" thread (omitted for a text or
  // image thread so their settings stay byte-identical, exactly like `kind` and
  // `image`). The store holds a concrete AudioSettings (chatDoc.ts) and both writers go
  // through audioSetting() so neither can reintroduce the key for another kind.
  audio?: ChatAudioSettings;
  // The speech-to-text knobs, present only on a "transcription" thread (omitted for a
  // text, image or speech thread so their settings stay byte-identical, exactly like
  // `kind`, `image` and `audio`). The store holds a concrete TranscriptionSettings
  // (chatDoc.ts) and both writers go through transcriptionSetting() so neither can
  // reintroduce the key for another kind.
  transcription?: ChatTranscriptionSettings;
};

// The full opaque content document the frontend stores per chat. Messages are
// kept loosely typed here (the concrete ChatUiMessage shape lives in the chat
// UI layer, above this transport layer).
export type ChatContentDoc = {
  settings: ChatSettings;
  messages: unknown[];
};

// Background chat runs: a run streams a single assistant turn server-side
// (surviving client disconnects) and is subscribed to via SSE elsewhere (the
// ChatStore); this transport layer only starts/cancels/lists runs.
export type ChatRunStatus = 'running' | 'completed' | 'error' | 'canceled' | 'interrupted';
export type ActiveChatRun = {
  chat_id: string;
  run_id: string;
  status: ChatRunStatus;
  // Present only for a run whose thread is pinned "image", "speech" or "transcription"
  // (all omitempty on the wire, mirroring activeRunDTO in chat_run_endpoints.go): the run's
  // kind and its server-measured age in ms. elapsed_ms is what lets a
  // REOPENED browser anchor the composer's pending clock on the same true
  // value the sending tab saw, rather than starting it over from zero (see
  // useChatRuns' elapsedMsOf and ImagePendingTurn).
  //
  // BOTH ARE READ. The bootstrap replay passes them straight into the run
  // engine (ChatStore's registerRunning + subscribeRun), where `kind` becomes
  // RunState.kind — the run's OWN, post-force kind, which is what the
  // in-flight turn renders from (ChatStore's runKind → ChatMessage's `kind`).
  // The thread-level ChatSettings.kind (chatKind/pinnedChatKind) decides what
  // the COMPOSER offers; this decides what the RUN is. They are two
  // questions, and a stale client can answer the first one wrongly.
  kind?: string;
  elapsed_ms?: number;
};
export type StartChatRunBody = {
  user_message?: unknown;
  edited_history?: unknown[];
  settings: {
    model: string;
    system_prompt: string;
    temperature: number;
    max_tokens: number;
    run_as_token_id: string;
    server_override: string;
    server_override_force_unreachable: boolean;
    // Mirrors ChatSettings.kind above (this shape is the run POST body, which
    // is a separate declaration of the same settings). On a thread's FIRST
    // send this value establishes the pin; afterwards the backend ignores it
    // in favour of the stored one.
    kind: string;
    // Mirrors ChatSettings.image: the image-generation knobs an image thread
    // sends. Absent for a text thread. The backend forwards the set fields to
    // /v1/images/generations (buildImagesBody) and clears them for a non-image
    // kind.
    image?: ChatImageSettings;
    // Mirrors ChatSettings.audio: the text-to-speech knobs a speech thread sends.
    // Absent for any other kind. The backend forwards the set fields to
    // /v1/audio/speech (buildSpeechBody) and clears them for a non-speech kind.
    audio?: ChatAudioSettings;
    // Mirrors ChatSettings.transcription: the speech-to-text knobs a transcription
    // thread sends. Absent for any other kind. The backend forwards the set fields as
    // form fields of /v1/audio/transcriptions (buildTranscriptionBody) and clears them
    // for a non-transcription kind.
    transcription?: ChatTranscriptionSettings;
  };
};
export type StartChatRunResponse = {
  run_id: string;
  chat_id: string;
  status: ChatRunStatus;
  // Mirrors ActiveChatRun's kind/elapsed_ms above (see startRunResponse's own
  // doc comment, chat_run_endpoints.go). elapsed_ms is ~0 here and, per its
  // own omitempty, in practice absent from this response -- that absence
  // says nothing, and the run engine's own default (0) is the true value for
  // a run that has genuinely just started.
  //
  // `kind` IS READ, and this is the response it matters most on: every send,
  // edit and regenerate passes it straight into subscribeRun (ChatStore), so
  // the pending turn renders from the kind the executor will ACTUALLY run --
  // the backend has already forced the thread's pinned kind onto it by this
  // point, which the client's own chatKind guess can contradict.
  kind?: string;
  elapsed_ms?: number;
};

export function chatApi(fetcher: Fetcher) {
  return {
    // Persistent chat playground documents (see ChatSummary / Chat above). The
    // list is ordered newest-updated first; content is decrypted only on the
    // single-chat GET. Mirrors the token/preferences method shapes.
    chats: () => request<ChatListResponse>(fetcher, '/api/portal/chats'),
    createChat: (body: { title?: string; content?: unknown }) =>
      request<Chat>(fetcher, '/api/portal/chats', { method: 'POST', body }),
    chat: (id: string) => request<Chat>(fetcher, `/api/portal/chats/${encodeURIComponent(id)}`),
    // `content` is OPTIONAL: the backend's PUT keeps the stored content when
    // the body carries no `content` key, so a title-only rename (renameChat,
    // ChatStore.tsx) writes just `{ title }`: it never uploads the local
    // transcript, and never overwrites the chat with it.
    saveChat: (id: string, body: { title: string; content?: unknown }) =>
      request<Chat>(fetcher, `/api/portal/chats/${encodeURIComponent(id)}`, {
        method: 'PUT',
        body,
      }),
    // Fire-and-forget counterpart to saveChat for the pagehide/unmount flush
    // (ChatStoreProvider): keepalive lets the PUT survive page unload, which
    // request()'s await+parse-response would defeat. Errors are swallowed --
    // the debounce/stream-end save via saveChat remains the primary path.
    saveChatKeepalive: (id: string, body: { title: string; content: unknown }): void => {
      void fetcher(`/api/portal/chats/${encodeURIComponent(id)}`, {
        method: 'PUT',
        keepalive: true,
        credentials: 'include',
        headers: { 'Content-Type': 'application/json', 'X-OP-CSRF': '1' },
        body: JSON.stringify(body),
      }).catch(() => {});
    },
    deleteChat: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/chats/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    // The two mutations of the audio answer lifecycle (#281). Both rewrite the chat's stored
    // document on the gateway (the audio parts become the `audio_removed` marker) and delete
    // the audio's blob rows in one atomic step, so the caller brings its own copy of the
    // transcript in line afterwards (ChatStore): a later saveChat of the stale copy would put
    // back an audio_url the gateway no longer serves. Both are refused with a 409
    // `portal.chat_run_active` while a run is live for the chat (the run owns the transcript),
    // and answer a 404 for an unknown or another owner's chat (and, for the delete, an unknown
    // message).
    //
    // Removes ONE answer's audio: every audio part of the message, however many blobs it spans.
    // A message with no stored audio is a successful no-op.
    deleteMessageAudio: (chatId: string, messageId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/chats/${encodeURIComponent(chatId)}/messages/${encodeURIComponent(messageId)}/audio`,
        { method: 'DELETE' },
      ),
    // Prunes the chat's stored audio down to the newest `keepLast` answers that hold any (the
    // retroactive half of the retention setting; the gateway prunes the same way at each run
    // commit). 0 removes every answer's audio. The gateway decides WHICH answers go from its own
    // stored document -- the count is the whole request -- and 400s a missing or negative one
    // (`portal.chat_audio_keep_last_invalid`), which this client never sends.
    pruneChatAudio: (chatId: string, keepLast: number) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/chats/${encodeURIComponent(chatId)}/audio/prune`,
        { method: 'POST', body: { keep_last: keepLast } },
      ),
    startChatRun: (chatId: string, body: StartChatRunBody) =>
      request<StartChatRunResponse>(
        fetcher,
        `/api/portal/chats/${encodeURIComponent(chatId)}/runs`,
        {
          method: 'POST',
          body,
        },
      ),
    cancelChatRun: (chatId: string, runId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/chats/${encodeURIComponent(chatId)}/runs/${encodeURIComponent(runId)}/cancel`,
        { method: 'POST' },
      ),
    activeChatRuns: () =>
      request<{ data: ActiveChatRun[] }>(fetcher, '/api/portal/chats/runs/active'),
  };
}
