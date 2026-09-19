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

// The chat listing plus the limits a client needs to stay inside them.
export type ChatListResponse = {
  data: ChatSummary[];
  // The backend's pre-seal content cap (portal.MaxChatContentBytes). Served
  // rather than duplicated here: a hardcoded copy would drift from the Go
  // constant with nothing to catch it. OPTIONAL because a gateway older than
  // this field simply omits it — the portal then treats the capacity as
  // unknown (states no number, refuses no send) instead of guessing one.
  max_content_bytes?: number;
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
  // chat) or "image". Established by the FIRST send and then FORCED by the
  // backend on every later send (PrepareChatRun), because the composer's
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
export type ActiveChatRun = { chat_id: string; run_id: string; status: ChatRunStatus };
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
  };
};
export type StartChatRunResponse = { run_id: string; chat_id: string; status: ChatRunStatus };

export function chatApi(fetcher: Fetcher) {
  return {
    // Persistent chat playground documents (see ChatSummary / Chat above). The
    // list is ordered newest-updated first; content is decrypted only on the
    // single-chat GET. Mirrors the token/preferences method shapes.
    chats: () => request<ChatListResponse>(fetcher, '/api/portal/chats'),
    createChat: (body: { title?: string; content?: unknown }) =>
      request<Chat>(fetcher, '/api/portal/chats', { method: 'POST', body }),
    chat: (id: string) => request<Chat>(fetcher, `/api/portal/chats/${encodeURIComponent(id)}`),
    saveChat: (id: string, body: { title: string; content: unknown }) =>
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
