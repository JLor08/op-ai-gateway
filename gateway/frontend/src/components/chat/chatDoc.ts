// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Pure, module-level helpers for the active-chat content document: types,
// localStorage-backed legacy migration, and small derivations (title, summary,
// ordering, run-metrics mapping). Nothing here touches React state or refs —
// extracted out of ChatStore.tsx (FA-2) so the provider is left with only the
// stateful run/persistence/CRUD logic. Behavior is unchanged; this is a pure
// relocation.

import type {
  Chat,
  ChatImageSettings,
  ChatRunStatus,
  ChatSettings,
  ChatSummary,
  PortalToken,
} from '../../api';
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
  // tps is the output CHARACTER rate ("chars/s"), kept under the legacy `tps`
  // name so old transcripts still render. tokensPerSecond is the real output
  // tokens/sec, present only once a turn has completed (issue #56).
  tps?: number;
  tokensPerSecond?: number;
  status?: ChatMessageStatus;
};

// Shape of the named SSE metrics payload (wire keys are snake_case; mapped to
// the camelCase message fields via metricsOf below).
export type RunMetricsPayload = {
  ttft_ms?: number;
  reasoning_ms?: number;
  tps?: number;
  tokens_per_second?: number;
};

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
  if (typeof metrics.tokens_per_second === 'number' && metrics.tokens_per_second > 0)
    out.tokensPerSecond = metrics.tokens_per_second;
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
  // No `kind` key: a text thread's persisted settings must not carry one at
  // all (see kindSetting below). DEFAULTS is written verbatim as a new chat's
  // content, so a `kind: ''` here would put the key on every chat at creation.
};

// Attachment cap: keep persisted transcripts well under the storage quota.
// Type/size validation lives in ../shared/imageAttach.
export const MAX_IMAGES = 5;

// The image-generation knobs the composer edits, as CONCRETE store state (every
// field defined, unlike the all-optional wire ChatImageSettings). An unset knob
// is the sentinel that imageSetting() below omits from the wire: "" for
// size/output_format ("leave sd-server's default"). Only the fields sd-server's
// OpenAI /v1/images/generations endpoint honors are here.
export type ImageSettings = {
  size: string; // "" = the model's own default
  n: number; // 1..IMAGE_N_MAX
  output_format: string; // "" = default (png) | "png" | "jpeg" | "webp"
  output_compression: number; // 0..100, only sent (and shown) for a lossy format
  // The advanced sd-server knobs (Phase 1b), reached via the <sd_cpp_extra_args>
  // channel. negative_prompt/sampler/scheduler are "" when unset. The numerics
  // are kept as BLANK-ABLE strings ("" = leave sd-server's own per-model default)
  // and parsed to numbers on the wire by imageSetting(); a blank or unparseable
  // one is omitted.
  negative_prompt: string;
  sampler: string; // "" = model default; one of IMAGE_SAMPLERS
  scheduler: string; // "" = model default; one of IMAGE_SCHEDULERS
  steps: string;
  cfg: string;
  seed: string;
  clip_skip: string;
};

export const DEFAULT_IMAGE_SETTINGS: ImageSettings = {
  size: '',
  n: 1,
  output_format: '',
  output_compression: 100,
  negative_prompt: '',
  sampler: '',
  scheduler: '',
  steps: '',
  cfg: '',
  seed: '',
  clip_skip: '',
};

// sd-server's batch_count ceiling for one request (its capabilities `limits`
// report max_batch_count: 8). Enforced by the composer; the gateway passes n
// through and sd-server clamps anyway.
export const IMAGE_N_MAX = 8;

// The size presets the composer offers. "" is "model default" (no size sent, so
// sd-server uses the loaded model's own). sd-server reads a free "WxH" string,
// so this list is a convenience, not a hard set.
export const IMAGE_SIZE_OPTIONS = [
  '',
  '512x512',
  '768x768',
  '1024x1024',
  '1024x1792',
  '1792x1024',
] as const;

// The output formats the generations endpoint accepts (png|jpeg|webp; webp only
// when the sd-server build has WebP support). "" leaves the default (png).
// output_compression (0..100) applies only to the two lossy formats.
export const IMAGE_OUTPUT_FORMATS = ['', 'png', 'jpeg', 'webp'] as const;

// sd-cpp's compiled-in sampler (sample_method_to_str) and scheduler
// (scheduler_to_str) enums, offered as a STATIC superset for Phase 1b. "" = the
// model's own default. The gateway forwards the pick opaquely, so a value a given
// sd-server build does not support is simply ignored upstream; a future Phase 1b
// would narrow these per model from /sdcpp/v1/capabilities.
export const IMAGE_SAMPLERS = [
  '',
  'euler',
  'euler_a',
  'heun',
  'dpm2',
  'dpm++2s_a',
  'dpm++2m',
  'dpm++2mv2',
  'ipndm',
  'ipndm_v',
  'lcm',
  'ddim_trailing',
  'tcd',
  'res_multistep',
  'res_2s',
  'er_sde',
  'euler_cfg_pp',
  'euler_a_cfg_pp',
  'euler_ge',
  'dpm++2m_sde',
  'dpm++2m_sde_bt',
  'lms',
] as const;

export const IMAGE_SCHEDULERS = [
  '',
  'discrete',
  'normal',
  'karras',
  'exponential',
  'ays',
  'gits',
  'sgm_uniform',
  'simple',
  'smoothstep',
  'kl_optimal',
  'lcm',
  'bong_tangent',
  'ltx2',
  'logit_normal',
  'flux2',
  'flux',
  'beta',
  'llada_image',
] as const;

// Parse a blank-able numeric knob string to a wire number, or undefined when
// blank/unparseable (so imageSetting omits it and sd-server keeps its default).
function toImageNumber(raw: string): number | undefined {
  const trimmed = raw.trim();
  if (trimmed === '') return undefined;
  const n = Number(trimmed);
  return Number.isFinite(n) ? n : undefined;
}

function toImageInt(raw: string): number | undefined {
  const n = toImageNumber(raw);
  return n === undefined ? undefined : Math.trunc(n);
}

// The inverse: a persisted wire number back to the store's blank-able string.
function imageNumToStr(n: number | undefined): string {
  return typeof n === 'number' && Number.isFinite(n) ? String(n) : '';
}
export const IMAGE_LOSSY_FORMATS = ['jpeg', 'webp'];

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

// The `kind` entry of a settings object, as a SPREADABLE fragment: present
// only when the kind is non-empty. This mirrors the backend's
// `json:"kind,omitempty"`, which exists so a text thread's persisted settings
// stay byte-identical to what they were before image threads existed (the
// backend has a test asserting the key never appears for one). Both builders
// of a settings object go through this, so neither can reintroduce the key
// from the client side: buildDoc in useChatPersistence, which writes the
// document, and normalizeDoc below, which seeds the state buildDoc writes
// back -- so a kind key normalizeDoc invents or drops is invented or dropped
// on the server too, by the next save.
export function kindSetting(kind: string | undefined): { kind?: string } {
  return kind ? { kind } : {};
}

// The `image` entry of a settings object, as a SPREADABLE fragment, mirroring
// kindSetting: present ONLY on an "image" thread, and carrying only the knobs
// the user moved off the default — so a text thread stays byte-identical and an
// untouched knob leaves sd-server on its own default. output_compression applies
// only to a lossy output_format (sd-server ignores it for png, where its control
// is hidden anyway), and like the others it is sent only when moved off the
// upstream default. Both the document writer (buildDoc) and the state seeder
// (normalizeDoc) go through this, so neither can give the key to a text thread
// from the client side.
export function imageSetting(
  kind: string | undefined,
  s: ImageSettings,
): { image?: ChatImageSettings } {
  if (kind !== 'image') return {};
  const image: ChatImageSettings = {};
  if (s.size) image.size = s.size;
  if (s.n > 0) image.n = s.n;
  if (s.output_format) image.output_format = s.output_format;
  if (
    IMAGE_LOSSY_FORMATS.includes(s.output_format) &&
    s.output_compression !== DEFAULT_IMAGE_SETTINGS.output_compression
  ) {
    image.output_compression = s.output_compression;
  }
  // Advanced knobs: strings/dropdowns sent when non-empty, blank-able numerics
  // parsed and sent only when a finite number. The gateway folds the set ones
  // into the <sd_cpp_extra_args> block.
  if (s.negative_prompt) image.negative_prompt = s.negative_prompt;
  if (s.sampler) image.sampler = s.sampler;
  if (s.scheduler) image.scheduler = s.scheduler;
  const steps = toImageInt(s.steps);
  if (steps !== undefined) image.steps = steps;
  const cfg = toImageNumber(s.cfg);
  if (cfg !== undefined) image.cfg = cfg;
  const seed = toImageInt(s.seed);
  if (seed !== undefined) image.seed = seed;
  const clipSkip = toImageInt(s.clip_skip);
  if (clipSkip !== undefined) image.clip_skip = clipSkip;
  return { image };
}

// Coerce a persisted (all-optional) ChatImageSettings into the concrete store
// state, filling anything missing/mistyped from the defaults. The inverse of
// imageSetting, used to seed the composer when a chat is loaded.
export function imageSettingsFrom(image: ChatImageSettings | undefined): ImageSettings {
  const s = image ?? {};
  return {
    size: typeof s.size === 'string' ? s.size : DEFAULT_IMAGE_SETTINGS.size,
    n: typeof s.n === 'number' && Number.isFinite(s.n) ? s.n : DEFAULT_IMAGE_SETTINGS.n,
    output_format:
      typeof s.output_format === 'string' ? s.output_format : DEFAULT_IMAGE_SETTINGS.output_format,
    output_compression:
      typeof s.output_compression === 'number' && Number.isFinite(s.output_compression)
        ? s.output_compression
        : DEFAULT_IMAGE_SETTINGS.output_compression,
    negative_prompt: typeof s.negative_prompt === 'string' ? s.negative_prompt : '',
    sampler: typeof s.sampler === 'string' ? s.sampler : '',
    scheduler: typeof s.scheduler === 'string' ? s.scheduler : '',
    steps: imageNumToStr(s.steps),
    cfg: imageNumToStr(s.cfg),
    seed: imageNumToStr(s.seed),
    clip_skip: imageNumToStr(s.clip_skip),
  };
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
      // The thread's pinned kind. It MUST be loaded here even though nothing
      // in normalizeDoc's own output interprets it: the save path rebuilds
      // `settings` from the state this seeds, and the backend's PUT
      // full-replaces the stored blob with no merge — so a setting this
      // function drops is a setting the next autosave silently erases from
      // the server. Passed through as an opaque string (no narrowing to the
      // kinds this build knows) for exactly the same reason, and spread
      // through kindSetting so an absent/empty one stays absent.
      ...kindSetting(typeof s.kind === 'string' ? s.kind : DEFAULTS.kind),
      // The image knobs, round-tripped for an image thread for the same reason
      // as kind: buildDoc rebuilds `settings` from the state this seeds, and the
      // PUT full-replaces the blob with no merge, so a knob dropped here is one
      // the next autosave erases. imageSetting keys off the kind, so a text
      // thread stays byte-identical (no image key).
      ...imageSetting(
        typeof s.kind === 'string' ? s.kind : DEFAULTS.kind,
        imageSettingsFrom(s.image),
      ),
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
      // A legacy single-conversation transcript predates image threads by
      // definition, so it migrates as text -- i.e. with no kind key at all.
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
