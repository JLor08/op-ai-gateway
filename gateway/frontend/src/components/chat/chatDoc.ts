// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Pure, module-level helpers for the active-chat content document: types,
// localStorage-backed legacy migration, and small derivations (title, summary,
// ordering, run-metrics mapping). Nothing here touches React state or refs —
// extracted out of ChatStore.tsx (FA-2) so the provider is left with only the
// stateful run/persistence/CRUD logic. Behavior is unchanged; this is a pure
// relocation.

import type { Chat, ChatRunStatus, ChatSettings, ChatSummary, PortalToken } from '../../api';
import type { ChatContent } from '../shared/chatContent';

// The persisted status of an assistant turn (mirrors the backend transcript
// vocabulary). `pending` is written by a server checkpoint while a run is live;
// `interrupted` is derived on reopen (Task 3.4) when a pending turn has no
// active run. Kept on every ChatUiMessage so it round-trips through save/load.
export type ChatMessageStatus = 'pending' | 'complete' | 'error' | 'canceled' | 'interrupted';

export type ChatUiMessage = {
  id: string;
  role: 'user' | 'assistant';
  content: ChatContent;
  reasoning?: string;
  reasoningMs?: number;
  ttftMs?: number;
  tps?: number;
  status?: ChatMessageStatus;
};

// Shape of the named SSE metrics payload (wire keys are snake_case; mapped to
// the camelCase message fields via metricsOf below).
export type RunMetricsPayload = { ttft_ms?: number; reasoning_ms?: number; tps?: number };

// resolveTokenOverride returns the run-as token's effective override for a
// requested model: an exact per-model map entry wins, otherwise the catch-all
// (model_override), otherwise "" (no override). Mirrors the backend's
// resolveModelOverride. Used only to display the effective model + gate the send;
// the server does the actual remap via the run-as token.
export function resolveTokenOverride(token: PortalToken | undefined, requested: string): string {
  if (!token) return '';
  const mapped = token.model_override_map?.[requested];
  if (mapped?.to) return mapped.to;
  return token.model_override ?? '';
}

// Map the wire metrics (snake_case) onto the UI message metric fields, dropping
// zero/absent values so the summary hides them until they are meaningful.
export function metricsOf(metrics?: RunMetricsPayload): Partial<ChatUiMessage> {
  const out: Partial<ChatUiMessage> = {};
  if (!metrics) return out;
  if (typeof metrics.ttft_ms === 'number' && metrics.ttft_ms > 0) out.ttftMs = metrics.ttft_ms;
  if (typeof metrics.reasoning_ms === 'number' && metrics.reasoning_ms > 0)
    out.reasoningMs = metrics.reasoning_ms;
  if (typeof metrics.tps === 'number' && metrics.tps > 0) out.tps = metrics.tps;
  return out;
}

// Terminal run status -> persisted assistant-message status.
export function messageStatusForRun(status: ChatRunStatus): ChatMessageStatus {
  switch (status) {
    case 'error':
      return 'error';
    case 'canceled':
      return 'canceled';
    case 'interrupted':
      return 'interrupted';
    default:
      return 'complete';
  }
}

// The active chat's opaque content document. Same shape as api.ChatContentDoc
// but with messages narrowed to the concrete UI message type.
export type ActiveChatDoc = { settings: ChatSettings; messages: ChatUiMessage[] };

// Legacy single-conversation localStorage keys (pre multi-chat). Read once on
// mount to migrate an existing conversation into the first server chat, then
// removed. No longer written.
export const LEGACY_KEYS = {
  model: 'op.chat.model',
  systemPrompt: 'op.chat.systemPrompt',
  temperature: 'op.chat.temperature',
  maxTokens: 'op.chat.maxTokens',
  messages: 'op.chat.messages',
  runAsToken: 'op.chat.runAsToken',
} as const;

// Tiny hint so a reload reopens the last active chat (best-effort).
export const ACTIVE_ID_KEY = 'op.chat.activeId';

// Debounced server-save cadence for the active chat.
export const SAVE_DEBOUNCE_MS = 800;

// Setting defaults when a chat's content omits them (or on a fresh chat).
export const DEFAULTS: ChatSettings = {
  model: '',
  system_prompt: '',
  temperature: 1,
  max_tokens: 0,
  run_as_token_id: '',
  server_override: '',
  server_override_force_unreachable: false,
};

// Attachment cap: keep persisted transcripts well under the storage quota.
// Type/size validation lives in ../shared/imageAttach.
export const MAX_IMAGES = 5;

// All localStorage access is guarded: some environments (incl. the jsdom test
// build) do not expose window.localStorage, and private-mode browsers can throw.
export function lsGet(key: string): string | null {
  try {
    return window.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

export function lsSet(key: string, value: string): void {
  try {
    window.localStorage?.setItem(key, value);
  } catch {
    /* best-effort */
  }
}

export function lsRemove(key: string): void {
  try {
    window.localStorage?.removeItem(key);
  } catch {
    /* best-effort */
  }
}

// Coerce an opaque `content` blob into a well-formed active-chat document,
// applying sane defaults for anything missing/mistyped.
export function normalizeDoc(content: unknown): ActiveChatDoc {
  const doc = (content ?? {}) as { settings?: Partial<ChatSettings>; messages?: unknown };
  const s = doc.settings ?? {};
  const messages = Array.isArray(doc.messages) ? (doc.messages as ChatUiMessage[]) : [];
  return {
    settings: {
      model: typeof s.model === 'string' ? s.model : DEFAULTS.model,
      system_prompt: typeof s.system_prompt === 'string' ? s.system_prompt : DEFAULTS.system_prompt,
      temperature:
        typeof s.temperature === 'number' && Number.isFinite(s.temperature)
          ? s.temperature
          : DEFAULTS.temperature,
      max_tokens:
        typeof s.max_tokens === 'number' && Number.isFinite(s.max_tokens)
          ? s.max_tokens
          : DEFAULTS.max_tokens,
      run_as_token_id:
        typeof s.run_as_token_id === 'string' ? s.run_as_token_id : DEFAULTS.run_as_token_id,
      server_override:
        typeof s.server_override === 'string' ? s.server_override : DEFAULTS.server_override,
      server_override_force_unreachable:
        typeof s.server_override_force_unreachable === 'boolean'
          ? s.server_override_force_unreachable
          : DEFAULTS.server_override_force_unreachable,
    },
    messages,
  };
}

// Read a meaningful legacy conversation for one-time migration. Returns null
// when there is nothing worth migrating (bare defaults / no messages).
export function readLegacyDoc(): ActiveChatDoc | null {
  const model = lsGet(LEGACY_KEYS.model) ?? '';
  const systemPrompt = lsGet(LEGACY_KEYS.systemPrompt) ?? '';
  const runAsToken = lsGet(LEGACY_KEYS.runAsToken) ?? '';
  const temperatureRaw = lsGet(LEGACY_KEYS.temperature);
  const maxTokensRaw = lsGet(LEGACY_KEYS.maxTokens);
  let messages: ChatUiMessage[] = [];
  const messagesRaw = lsGet(LEGACY_KEYS.messages);
  if (messagesRaw) {
    try {
      const parsed = JSON.parse(messagesRaw);
      if (Array.isArray(parsed)) messages = parsed as ChatUiMessage[];
    } catch {
      /* ignore corrupt transcript */
    }
  }
  const meaningful =
    messages.length > 0 || model !== '' || systemPrompt !== '' || runAsToken !== '';
  if (!meaningful) return null;
  const num = (raw: string | null, fallback: number) => {
    if (raw === null || raw === '') return fallback;
    const parsed = Number(raw);
    return Number.isFinite(parsed) ? parsed : fallback;
  };
  return {
    settings: {
      model,
      system_prompt: systemPrompt,
      temperature: num(temperatureRaw, DEFAULTS.temperature),
      max_tokens: num(maxTokensRaw, DEFAULTS.max_tokens),
      run_as_token_id: runAsToken,
      server_override: DEFAULTS.server_override,
      server_override_force_unreachable: DEFAULTS.server_override_force_unreachable,
    },
    messages,
  };
}

export function clearLegacyKeys(): void {
  for (const key of Object.values(LEGACY_KEYS)) lsRemove(key);
}

// First user message text -> a short single-line title (~40 chars).
function textOf(content: ChatContent): string {
  if (typeof content === 'string') return content;
  const textPart = content.find((part) => part.type === 'text');
  return textPart && 'text' in textPart ? textPart.text : '';
}

export function deriveTitle(text: string): string {
  const singleLine = text.replace(/\s+/g, ' ').trim();
  return singleLine.length > 40 ? singleLine.slice(0, 40).trimEnd() : singleLine;
}

export function deriveTitleFromMessages(messages: ChatUiMessage[]): string {
  const firstUser = messages.find((message) => message.role === 'user');
  return firstUser ? deriveTitle(textOf(firstUser.content)) : '';
}

export function summaryOf(chat: Chat): ChatSummary {
  return {
    id: chat.id,
    title: chat.title,
    created_at: chat.created_at,
    updated_at: chat.updated_at,
  };
}

function compareUpdatedDesc(a: ChatSummary, b: ChatSummary): number {
  if (a.updated_at < b.updated_at) return 1;
  if (a.updated_at > b.updated_at) return -1;
  return 0;
}

// Newest-updated first, matching the backend list ordering.
export function byNewest(list: ChatSummary[]): ChatSummary[] {
  return [...list].sort(compareUpdatedDesc);
}

// Drop a dangling empty assistant placeholder (empty content AND no reasoning) —
// left behind when a stream ends before the first content token (error frame,
// or Stop / view-change before any output). Returns the same array reference
// when there is nothing to prune. content is a string for assistant turns;
// `.length` covers both the string and (defensively) the array shape.
export function pruneEmptyAssistantTail(list: ChatUiMessage[]): ChatUiMessage[] {
  const last = list.at(-1);
  const empty = last?.role === 'assistant' && last.content.length === 0 && !last.reasoning;
  return empty ? list.slice(0, -1) : list;
}

// Whether a replayed message history carries an image attachment — used by
// startRunWithHistory's vision gate (edit/regenerate on a non-vision model
// must not resend an image the model can't process).
export function historyHasImage(messages: ChatUiMessage[]): boolean {
  return messages.some(
    (m) => Array.isArray(m.content) && m.content.some((p) => p.type === 'image_url'),
  );
}

let idSeq = 0;
export function nextId(): string {
  const cryptoObj = globalThis.crypto as Crypto | undefined;
  if (cryptoObj?.randomUUID) return cryptoObj.randomUUID();
  idSeq += 1;
  return `msg-${idSeq}`;
}
