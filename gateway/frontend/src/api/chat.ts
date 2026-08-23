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
  };
};
export type StartChatRunResponse = { run_id: string; chat_id: string; status: ChatRunStatus };

export function chatApi(fetcher: Fetcher) {
  return {
    // Persistent chat playground documents (see ChatSummary / Chat above). The
    // list is ordered newest-updated first; content is decrypted only on the
    // single-chat GET. Mirrors the token/preferences method shapes.
    chats: () => request<{ data: ChatSummary[] }>(fetcher, '/api/portal/chats'),
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
