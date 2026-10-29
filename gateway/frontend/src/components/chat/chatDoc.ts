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
  ChatAudioSettings,
  ChatImageSettings,
  ChatRunStatus,
  ChatSettings,
  ChatSummary,
  ChatTranscriptionSettings,
  ChatVoiceChatSettings,
  PortalToken,
} from '../../api';
import {
  audioUrlsOf,
  isMaskPart,
  maskPart,
  type ChatContent,
  type ChatContentPart,
} from '../shared/chatContent';
import type { SpokenSection } from '../shared/spokenSections';

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
  // The spoken sections of a VOICE answer (#315), written by the gateway beside the full clean
  // answer in `content` and omitted when there were none: an ordered list of spans into the
  // answer's text (RUNE offsets `start`/`len`; `text` is the spoken text, kept beside the span and
  // optional). Each renders as its own collapsed, replayable block at its position; its audio is
  // the section's parts in `content`, paired by order (the committed list holds only sections
  // that were spoken). Turns committed before sections existed carry the scalar spoken_summary
  // below instead, and the portal reads these over it when both are there.
  spoken_sections?: SpokenSection[];
  // The text of a VOICE answer's spoken summary (#314), written by the gateway beside the full
  // clean answer in `content` (summary and detail both) and omitted when there was none. The
  // portal renders it as the collapsed, replayable block. Only turns committed before spoken
  // sections (#315) carry it; newer turns write spoken_sections instead. snake_case because it is
  // the chat document's own key, not one of the camelCase run metrics above.
  spoken_summary?: string;
  // The RUNE offset of spoken_summary within the answer text (#322), written by the gateway beside
  // it so the collapsed block's body is sliced out by POSITION rather than re-found by value --
  // exact even when the summary's text repeats before the <speak> region. Present only on turns
  // committed with it; an older turn omits it and the portal falls back to the by-value removal.
  spoken_summary_start?: number;
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

// What an image attached to the composer MEANS for the next send (ADR-043 R1):
//
//   'vision' -- an input the picked text model reads;
//   'edit'   -- the source image of an image EDIT: the thread is an image thread
//               whose picked model may be edited, and the server turns a turn whose
//               last user message carries an image into an edit of it;
//   'none'   -- nothing: the control is disabled and an attachment is refused or
//               cleared.
//
// It is ONE predicate of the thread's kind and the picked model's flags, and the
// attach control, the clear on a model switch, send() and the replay guard all
// read it, so they cannot disagree. The THREAD's kind decides first -- a text
// thread stays a text thread and an image thread an image thread whichever model
// is picked afterwards -- and `vision` is deliberately not consulted for an image
// thread: an sd model is vision:false and still edits. On a text thread the old
// rule stands: a vision model attaches, but a model that also GENERATES images
// does not (an image model has no use for input on a text thread).
export type AttachMode = 'edit' | 'vision' | 'none';

export function attachModeOf(
  kind: string,
  model: { vision: boolean; image: boolean; imageEdit: boolean },
): AttachMode {
  if (kind === 'image') return model.imageEdit ? 'edit' : 'none';
  // Text-to-speech takes no image input whatever the model can otherwise read: a
  // speech run sends only the last user message's text, so an attachment would be
  // dropped in silence. (Without this the vision branch below would offer one.)
  if (kind === 'speech') return 'none';
  // Likewise speech-to-text: a transcription run reads only the audio of the last user
  // message, so an attached image would be dropped in silence.
  if (kind === 'transcription') return 'none';
  return model.vision && !model.image ? 'vision' : 'none';
}

// The MASK of an image edit is an image part tagged `purpose: 'mask'` (the tag and
// its predicate, MASK_PURPOSE / maskPart / isMaskPart, are defined with the part
// shape in ../shared/chatContent -- the message renderer needs them too -- and
// re-exported here for the chat module). A mask is only ever the mask OF an edit.
export { MASK_PURPOSE, isMaskPart, maskPart } from '../shared/chatContent';

// The content of the user turn the composer sends: the text alone when no image is
// attached, otherwise the text, the attached image(s) and -- for an edit of exactly
// one source image only -- the mask. A mask without a source, or outside an edit
// (a text model reading an image has no use for one), is left out rather than
// sent: it could mean nothing there, and the executor would ignore it anyway.
export function userTurnContent(
  text: string,
  images: string[],
  mask: string | null,
  mode: AttachMode,
): ChatContent {
  if (images.length === 0) return text;
  const parts: ChatContentPart[] = [
    { type: 'text', text },
    ...images.map((url): ChatContentPart => ({ type: 'image_url', image_url: { url } })),
  ];
  if (mask !== null && mode === 'edit' && images.length === 1) parts.push(maskPart(mask));
  return parts;
}

// The url a stored image is served from: /api/portal/chat-blobs/{id}, owner-scoped to
// the chat that holds the blob. A transcript's image part is one of these once its
// run is committed (images live out of the document), and a refine reuses it as the
// source of the next edit. Bound to ITS chat, unlike an inline data url, which is just
// bytes and goes wherever the composer goes.
const CHAT_BLOB_URL_PREFIX = '/api/portal/chat-blobs/';

export function isChatBlobUrl(url: string): boolean {
  return url.startsWith(CHAT_BLOB_URL_PREFIX);
}

// The formats the edit route of sd-server reads a source image in. The composer
// takes no other one for an edit (the picker's `accept`, and handleFiles, which an
// `accept` cannot bind), so a webp or gif source is refused where the user can
// still act on it instead of coming back from the upstream as a generic error.
// Lower case: compared against a file's reported MIME type.
//
// HEIC/HEIF are the one exception: they are not a format sd-server would see, because
// the upload pipeline (prepareImageDataUrl) converts them to JPEG before they are
// attached, so the guard lets them through (isHeicFile, which also recognises one a
// browser reports no MIME type for by its extension) and the picker lists them -- the
// extensions too, since a file dialog matches those when it has no MIME type to go by.
export const IMAGE_EDIT_SOURCE_TYPES: readonly string[] = ['image/png', 'image/jpeg'];
export const IMAGE_EDIT_SOURCE_ACCEPT = [
  ...IMAGE_EDIT_SOURCE_TYPES,
  'image/heic',
  'image/heif',
  '.heic',
  '.heif',
].join(',');

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
  // The EDIT knob: how far an image edit departs from its source image. Blank-able
  // like the numerics above ("" = sd-server's own default, 0.75, which makes a weak
  // edit); the gateway delivers a set one in the same <sd_cpp_extra_args> block.
  // sd-server ignores it for a plain generation, so it lives with the rest of the
  // thread's settings.
  strength: string;
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
  strength: '',
};

// The portal's HARD cap on images per request: a portal-side limit (it dates from
// when every generated image counted against the transcript's cap; the images now
// live in chat_blobs and the cap was left as it was), not an sd-server limit. A
// model's reported max batch count (GET /api/portal/model-image-options) may only
// NARROW the composer's count below this, never raise it. Enforced by the
// composer's count field (Chat.tsx).
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
// (scheduler_to_str) enums, kept as a STATIC superset. "" = the model's own
// default. They are the fallback the composer uses when GET
// /api/portal/model-image-options returns nothing for a field; the picked
// model's own options narrow them when it does. The gateway forwards the pick
// opaquely, so a value a given sd-server build does not support is simply
// ignored upstream.
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

// The merge rule for a per-model option list (GET /api/portal/model-image-options):
// the fetched list wins when it is non-empty, else the static superset above. The
// endpoint omits (omitempty) a list it does not know, so absent and empty both mean
// "unknown" and the dropdown must still offer something.
export function optionsOr(
  fetched: readonly string[] | undefined,
  staticList: readonly string[],
): readonly string[] {
  return fetched && fetched.length > 0 ? fetched : staticList;
}

// Parse a blank-able numeric knob string to a wire number, or undefined when
// blank/unparseable (so imageSetting omits it and sd-server keeps its default).
// Shared with the audio speed knob (audioSetting) and the transcription temperature
// (transcriptionSetting), which are blank-able the same way.
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
  const strength = toImageNumber(s.strength);
  if (strength !== undefined) image.strength = strength;
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
    strength: imageNumToStr(s.strength),
  };
}

// The text-to-speech knobs the composer edits, as CONCRETE store state (every field
// defined, unlike the all-optional wire ChatAudioSettings). Every unset knob is the
// blank sentinel that audioSetting() below omits from the wire, so the model keeps its
// own default (its default voice is shown as a placeholder, never written).
export type AudioSettings = {
  voice: string; // picked or typed (combobox); "" = the model's default voice
  // The speaking-rate multiplier as a BLANK-ABLE string ("" = the model's own default),
  // parsed to a number on the wire by audioSetting(); a blank or unparseable one is
  // omitted. Not clamped: the model's own range applies.
  speed: string;
  // The language to speak in, free text (an ISO-639-1-ish code such as "de"): typed by the
  // user or pre-filled with the portal's UI language where the model takes one (the composer's
  // control exists only then). "" = the model's own default; audioSetting() trims it and omits
  // it when blank, so an empty string is never sent.
  language: string;
  // The advanced passthrough as the TEXT of a JSON object (the composer's textarea):
  // extra top-level request fields merged verbatim into /v1/audio/speech -- the OpenAI
  // `instructions` field and any audio.cpp-native knob -- each value keeping the JSON
  // type the user wrote, which is why this is JSON and not key=value lines. "" = none.
  // audioSetting() sends it parsed, or omits it when it is not a non-empty object free
  // of reserved fields; parseAudioPassthrough() says which, so the composer and send()
  // can name a bad one.
  passthrough: string;
  // Whether the thread plays a streaming-capable model's audio while it is generated
  // (ADR-068), the speech composer's "stream" toggle. On by default: the run streams
  // wherever the gateway's gate allows it (the model streams and its output rate is known)
  // and the toggle only appears for such a model; it is the user's way to opt a thread out
  // and get the buffered WAV instead. audioSetting() writes it only when it is OFF, so a
  // thread that never touched it persists nothing for it.
  stream: boolean;
  // Whether the thread stores its spoken answers' audio at all (#281), the speech composer's
  // "keep audio" toggle. On by default, so a new or legacy chat keeps every answer, as before.
  // OFF is the wire's `keep_last_audio: 0` (do not store); audioSetting() writes it only then.
  keepAudio: boolean;
  // How many of the most recent answers keep their audio, as a BLANK-ABLE string like `speed`
  // ("" = keep all), parsed to the wire's positive integer by audioSetting(); a blank,
  // non-integer or sub-1 text is omitted. Only meaningful while keepAudio is on: turning it
  // off wins and this text is not sent.
  keepLast: string;
};

export const DEFAULT_AUDIO_SETTINGS: AudioSettings = {
  voice: '',
  speed: '',
  language: '',
  passthrough: '',
  stream: true,
  keepAudio: true,
  keepLast: '',
};

// The request fields a thread's passthrough may not set: the run's own (`model` is
// the thread's pinned model, `input` the user's text) and the three its one-complete-WAV
// turn depends on (`stream` and `stream_format`, which the run states itself when it
// streams and omits when it does not, and `response_format`).
// This mirrors speechReservedFields in the backend's chat_runs_audio.go -- keep the
// two in step. The backend still refuses such a key (gateway.chat_run_speech_reserved_field,
// matched ignoring case), but only AFTER the turn is committed; this copy lets the
// composer refuse it inline, before a turn is sent. Keys compare lower-cased, as the
// backend's strings.EqualFold does for the ASCII names here.
export const SPEECH_RESERVED_FIELDS: readonly string[] = [
  'model',
  'input',
  'stream',
  'stream_format',
  'response_format',
];

// What the passthrough text is: nothing to send ('empty': blank or `{}`), a JSON
// object to send ('ok') or something that cannot be sent ('invalid'): malformed JSON,
// valid JSON that is not an object (an array, a string, a number, null), or an object
// naming a field the run owns (SPEECH_RESERVED_FIELDS), in which case `reservedField`
// is the first such key as the user wrote it, so the UI can name it.
//
// `badKey` is the one refusal that only a MULTIPART passthrough has (the transcription one): a key
// that cannot be a form field name, as the user wrote it ('' for an empty key, so test it against
// undefined, not for truthiness). See isFormFieldName.
export type AudioPassthroughParse =
  | { status: 'empty' }
  | { status: 'ok'; value: Record<string, unknown> }
  | { status: 'invalid'; reservedField?: string; badKey?: string };

export function parseAudioPassthrough(raw: string): AudioPassthroughParse {
  return parsePassthrough(raw, SPEECH_RESERVED_FIELDS, false);
}

// Whether a passthrough key can be the NAME of a multipart form part: non-empty, free of control
// characters (a line break would end the part's header line and begin another), a double quote and
// a backslash (the name sits between quotes in `Content-Disposition: form-data; name="..."`, and
// the upstream's form parser, cpp-httplib, reads it with a pattern a quote or a backslash cuts
// short). This mirrors the backend's isTranscriptionFieldName in chat_runs_transcription.go, which
// refuses an empty or control-character key and lets a quote or a backslash through (its writer
// escapes them, which that upstream parser does not undo), so this side refuses those too -- keep
// the two in step. The speech passthrough is a JSON body, where any key is fine, so it does not ask.
function isFormFieldName(key: string): boolean {
  if (key === '' || key.includes('"') || key.includes('\\')) return false;
  for (const char of key) {
    const code = char.codePointAt(0) ?? 0;
    if (code < 0x20 || code === 0x7f) return false;
  }
  return true;
}

// The one parse of a passthrough text, for the thread kinds that have one: they differ only
// in which fields their run owns (`reserved` holds the lower-case names) and in whether the keys
// become form field names (`formFields`).
function parsePassthrough(
  raw: string,
  reserved: readonly string[],
  formFields: boolean,
): AudioPassthroughParse {
  const text = raw.trim();
  if (text === '') return { status: 'empty' };
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { status: 'invalid' };
  }
  if (!isJsonObject(parsed)) return { status: 'invalid' };
  const keys = Object.keys(parsed);
  // Scan a sorted copy, like the backend's, so the field named is deterministic;
  // `keys` keeps its original order for the length check below.
  const sorted = [...keys].sort((a, b) => a.localeCompare(b));
  const reservedField = sorted.find((key) => reserved.includes(key.toLowerCase()));
  if (reservedField !== undefined) return { status: 'invalid', reservedField };
  if (formFields) {
    const badKey = sorted.find((key) => !isFormFieldName(key));
    if (badKey !== undefined) return { status: 'invalid', badKey };
  }
  return keys.length === 0 ? { status: 'empty' } : { status: 'ok', value: parsed };
}

function isJsonObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// The message for a passthrough text that parseAudioPassthrough called invalid: the
// reserved-field one names the offending field, any other says it must be a JSON
// object. Takes only the two keys it reads so chatDoc stays free of the i18n module.
export function audioPassthroughMessage(
  parse: AudioPassthroughParse,
  t: {
    chatAudioPassthroughInvalid: string;
    chatSpeechPassthroughReserved: (field: string) => string;
  },
): string {
  return parse.status === 'invalid' && parse.reservedField !== undefined
    ? t.chatSpeechPassthroughReserved(parse.reservedField)
    : t.chatAudioPassthroughInvalid;
}

// Whether a run of this thread must be refused for its advanced passthrough: a speech
// thread whose passthrough text is not a JSON object (audioSetting would omit it, and
// the user's extra fields would vanish from the request in silence -- the Advanced
// section may be collapsed when they send) or names a reserved field (the run would
// commit the turn and then fail it server-side). The store refuses the send (and a
// replay) and says so instead. Only a speech thread has a passthrough.
export function speechPassthroughInvalid(kind: string | undefined, s: AudioSettings): boolean {
  return kind === 'speech' && parseAudioPassthrough(s.passthrough).status === 'invalid';
}

// The `audio` entry of a settings object, as a SPREADABLE fragment, mirroring
// imageSetting: present ONLY on a "speech" thread, and carrying only what the user
// set -- so a text or image thread stays byte-identical and an untouched knob leaves
// the model on its own default. (The key is present for a speech thread even when
// nothing is set, as image's is for an image thread: `audio: {}`.) A passthrough that
// is not a sendable non-empty JSON object is omitted here; the store refuses the turn before it
// gets that far (speechPassthroughInvalid), so it is never dropped in silence. Both the
// document writer (buildDoc) and the state seeder (normalizeDoc) go through this, so
// neither can give the key to another kind of thread from the client side.
export function audioSetting(
  kind: string | undefined,
  s: AudioSettings,
): { audio?: ChatAudioSettings } {
  if (kind !== 'speech') return {};
  const audio: ChatAudioSettings = {};
  const voice = s.voice.trim();
  if (voice) audio.voice = voice;
  const speed = toImageNumber(s.speed);
  if (speed !== undefined) audio.speed = speed;
  const language = s.language.trim();
  if (language) audio.language = language;
  const passthrough = parseAudioPassthrough(s.passthrough);
  if (passthrough.status === 'ok') audio.passthrough = passthrough.value;
  // Only the opt-out is written: streaming is the default, so an unset thread persists
  // nothing and a thread's settings stay byte-identical until the user turns it off.
  if (!s.stream) audio.stream = false;
  // Likewise only the non-default retention: do-not-store (0) when keep-audio is off, else a
  // positive keep-last-N. Keep-all (the default, or a blank/invalid keepLast) writes nothing.
  const keepLast = keepLastAudioOf(s);
  if (keepLast !== undefined) audio.keep_last_audio = keepLast;
  return { audio };
}

// The `keep_last_audio` these settings put on the wire: 0 (do not store), N >= 1 (keep the
// last N answers' audio), or undefined (keep every answer: the default, and what a blank or
// unusable keepLast means). THE parse of the retention -- audioSetting writes exactly this --
// so anything that has to reason about what a setting DOES (the store's "does this change
// tighten the retention?") reads it here and never the raw keepLast text, which can say
// something the wire drops.
export function keepLastAudioOf(s: AudioSettings): number | undefined {
  return s.keepAudio ? toKeepLast(s.keepLast) : 0;
}

// The number of answers the settings keep the audio of, as a comparable number: Infinity for
// keep-all, else keepLastAudioOf's count (0 for do-not-store).
function audioRetentionLimit(s: AudioSettings): number {
  return keepLastAudioOf(s) ?? Number.POSITIVE_INFINITY;
}

// Whether changing `current` into `next` keeps the audio of FEWER answers: a smaller N,
// do-not-store, or keep-all narrowed to a finite N. Judged on what the wire would carry
// (keepLastAudioOf), so a blank or invalid count -- which the wire drops, i.e. keep-all -- is
// never a tightening, and neither is raising N, keep-all or storing audio again. The case
// that asks the user before it deletes anything (the retroactive prune).
export function lowersAudioRetention(current: AudioSettings, next: AudioSettings): boolean {
  return audioRetentionLimit(next) < audioRetentionLimit(current);
}

// Whether a message is an ANSWER holding stored audio: an assistant message with at least one
// audio_url part. What the gateway's retention counts, prunes and deletes (chat_audio_retention.go:
// isAssistantMessage + messageHasStoredAudio) and what this side mirrors -- a user turn's audio
// is a transcription clip the user supplied and is never an answer's, a text answer has none, and
// a not-stored answer (the audio_removed marker) no longer holds any, so it takes no slot.
export function holdsStoredAudio(message: ChatUiMessage): boolean {
  return message.role === 'assistant' && audioUrlsOf(message.content).length > 0;
}

// The not-stored state of one answer, as the gateway rewrites it: every audio_url part gone,
// one `audio_removed` marker after the parts that stay (the text of a mixed answer keeps its
// place), everything else about the message as it was. The very same object comes back for a
// message that holds no stored audio, so applying it twice, or to a text answer or a user
// turn, changes nothing.
export function withAudioRemoved(message: ChatUiMessage): ChatUiMessage {
  if (!holdsStoredAudio(message) || typeof message.content === 'string') return message;
  const kept = message.content.filter((part) => part.type !== 'audio_url');
  const marked = kept.some((part) => part.type === 'audio_removed');
  return { ...message, content: marked ? kept : [...kept, { type: 'audio_removed' }] };
}

// The answers of a transcript that hold audio beyond the newest `keepLast` of them, i.e. the
// ones a prune to that count would turn into the not-stored state. keepLast 0 counts every
// one; a negative count is no count and prunes nothing.
export function audioAnswersBeyond(messages: readonly ChatUiMessage[], keepLast: number): number {
  if (keepLast < 0) return 0;
  const holding = messages.filter(holdsStoredAudio).length;
  return Math.max(0, holding - keepLast);
}

// The transcript pruned to the newest `keepLast` answers that hold audio: every older one is
// rewritten into the not-stored state (withAudioRemoved), in place. The gateway's own walk
// (pruneAudioBeyond in chat_audio_retention.go) -- newest to oldest over the assistant answers
// that hold audio, the newest `keepLast` kept -- run over the local copy once the gateway has
// pruned its stored one, so the two agree and a later save cannot write back audio the gateway
// deleted. The very same array comes back when nothing is beyond the limit.
export function pruneAudioBeyond(messages: ChatUiMessage[], keepLast: number): ChatUiMessage[] {
  if (audioAnswersBeyond(messages, keepLast) === 0) return messages;
  let kept = 0;
  const out = [...messages];
  for (let i = out.length - 1; i >= 0; i--) {
    if (!holdsStoredAudio(out[i])) continue;
    if (kept < keepLast) kept++;
    else out[i] = withAudioRemoved(out[i]);
  }
  return out;
}

// The positive integer a keep-last text means, or undefined for keep-all: blank, unparseable,
// fractional, or below 1 (0 is the do-not-store state, which the keepAudio toggle carries,
// not this text).
function toKeepLast(raw: string): number | undefined {
  const n = toImageNumber(raw);
  return n !== undefined && Number.isSafeInteger(n) && n >= 1 ? n : undefined;
}

// Whether a keep-last text is filled in but unusable (not a whole number of 1 or more): the
// case audioSetting() drops in silence (keep-all), so the composer can say so beside the
// field. A blank text is not invalid, it is simply not filled in yet. Shares toKeepLast, so
// the warning and the wire can never disagree about what a usable count is.
export function isKeepLastInvalid(raw: string): boolean {
  return raw.trim() !== '' && toKeepLast(raw) === undefined;
}

// Coerce a persisted (all-optional) ChatAudioSettings into the concrete store state,
// filling anything missing/mistyped from the defaults. The inverse of audioSetting,
// used to seed the composer when a chat is loaded: the passthrough object comes back
// as pretty-printed JSON text for the textarea.
export function audioSettingsFrom(audio: ChatAudioSettings | undefined): AudioSettings {
  const s = audio ?? {};
  const passthrough =
    isJsonObject(s.passthrough) && Object.keys(s.passthrough).length > 0
      ? JSON.stringify(s.passthrough, null, 2)
      : DEFAULT_AUDIO_SETTINGS.passthrough;
  return {
    voice: typeof s.voice === 'string' ? s.voice : DEFAULT_AUDIO_SETTINGS.voice,
    speed: imageNumToStr(s.speed),
    language: typeof s.language === 'string' ? s.language : DEFAULT_AUDIO_SETTINGS.language,
    passthrough,
    // Anything but an explicit false is the default (on): a missing or mistyped value must
    // not silently turn streaming off.
    stream: s.stream !== false,
    ...keepAudioFrom(s.keep_last_audio),
  };
}

// The retention half of the audio state from the persisted `keep_last_audio`: 0 = do not
// store, an integer >= 1 = keep the last N, anything else (absent, negative, fractional,
// mistyped) = keep all, the default -- a stray value must not silently stop storing audio.
function keepAudioFrom(keep: unknown): Pick<AudioSettings, 'keepAudio' | 'keepLast'> {
  if (keep === 0) return { keepAudio: false, keepLast: '' };
  if (typeof keep === 'number' && Number.isSafeInteger(keep) && keep >= 1) {
    return { keepAudio: true, keepLast: String(keep) };
  }
  return { keepAudio: DEFAULT_AUDIO_SETTINGS.keepAudio, keepLast: DEFAULT_AUDIO_SETTINGS.keepLast };
}

// The speech-to-text knobs the composer edits, as CONCRETE store state (every field
// defined, unlike the all-optional wire ChatTranscriptionSettings): the speech settings'
// twin. Every unset knob is the blank sentinel that transcriptionSetting() below omits
// from the wire, so the model keeps its own default.
export type TranscriptionSettings = {
  language: string; // free text (an ISO-639-1 code); "" = the model detects it
  prompt: string; // free text; "" = none
  // The sampling temperature as a BLANK-ABLE string ("" = the model's own default), parsed
  // to a number on the wire by transcriptionSetting(); a blank or unparseable one is
  // omitted. A typed 0 is a value (deterministic decoding), not blank. Not clamped: the
  // model's own range applies.
  temperature: string;
  // The advanced passthrough as the TEXT of a JSON object (the composer's textarea): extra
  // request fields merged verbatim into the /v1/audio/transcriptions form -- the OpenAI
  // timestamp granularities and any audio.cpp-native knob -- each value keeping the JSON
  // type the user wrote. "" = none. transcriptionSetting() sends it parsed, or omits it
  // when it is not a non-empty object free of reserved fields; parseTranscriptionPassthrough()
  // says which, so the composer and send() can name a bad one.
  passthrough: string;
};

export const DEFAULT_TRANSCRIPTION_SETTINGS: TranscriptionSettings = {
  language: '',
  prompt: '',
  temperature: '',
  passthrough: '',
};

// The form fields a transcription thread's passthrough may not set: the run's own (`model`
// is the thread's pinned model, `file` the user's clip) and the two its one-complete-JSON
// contract depends on (`response_format`, which the gateway forces to json, and `stream`,
// which the endpoint refuses outright). This mirrors transcriptionReservedFields in the
// backend's chat_runs_transcription.go -- keep the two in step. The backend still refuses
// such a key (gateway.chat_run_transcription_reserved_field, matched ignoring case), but only
// AFTER the turn is committed; this copy lets the composer refuse it inline, before a turn
// is sent. Keys compare lower-cased, as the backend's strings.EqualFold does for the ASCII
// names here.
export const TRANSCRIPTION_RESERVED_FIELDS: readonly string[] = [
  'model',
  'file',
  'response_format',
  'stream',
];

// What the transcription passthrough text is: the same classification as the speech one
// (parseAudioPassthrough, hence the same AudioPassthroughParse), against this run's reserved
// fields.
export function parseTranscriptionPassthrough(raw: string): AudioPassthroughParse {
  return parsePassthrough(raw, TRANSCRIPTION_RESERVED_FIELDS, true);
}

// The message for a passthrough text that parseTranscriptionPassthrough called invalid: the
// reserved-field one names the offending field, the bad-key one names the key (JSON-escaped, so a
// quote, a backslash or a line break in it is readable instead of cutting the sentence), any other
// says it must be a JSON object. Takes only the three keys it reads so chatDoc stays free of the
// i18n module.
export function transcriptionPassthroughMessage(
  parse: AudioPassthroughParse,
  t: {
    chatTranscriptionPassthroughInvalid: string;
    chatTranscriptionPassthroughReserved: (field: string) => string;
    chatTranscriptionPassthroughBadKey: (field: string) => string;
  },
): string {
  if (parse.status === 'invalid' && parse.reservedField !== undefined) {
    return t.chatTranscriptionPassthroughReserved(parse.reservedField);
  }
  if (parse.status === 'invalid' && parse.badKey !== undefined) {
    return t.chatTranscriptionPassthroughBadKey(JSON.stringify(parse.badKey));
  }
  return t.chatTranscriptionPassthroughInvalid;
}

// Whether a run of this thread must be refused for its advanced passthrough: a transcription
// thread whose passthrough text is not a JSON object (transcriptionSetting would omit it, and
// the user's extra fields would vanish from the request in silence -- the Advanced section may
// be collapsed when they send) or names a reserved field (the run would commit the turn and
// then fail it server-side). The store refuses the send (and a replay) and says so instead.
// Only a transcription thread has this passthrough.
export function transcriptionPassthroughInvalid(
  kind: string | undefined,
  s: TranscriptionSettings,
): boolean {
  return (
    kind === 'transcription' && parseTranscriptionPassthrough(s.passthrough).status === 'invalid'
  );
}

// The `transcription` entry of a settings object, as a SPREADABLE fragment, mirroring
// audioSetting: present ONLY on a "transcription" thread, and carrying only what the user
// set -- so a text, image or speech thread stays byte-identical and an untouched knob leaves
// the model on its own default. (The key is present for a transcription thread even when
// nothing is set, as audio's is for a speech thread: `transcription: {}`.) A passthrough that
// is not a sendable non-empty JSON object is omitted here; the store refuses the turn before
// it gets that far (transcriptionPassthroughInvalid), so it is never dropped in silence. Both
// the document writer (buildDoc) and the state seeder (normalizeDoc) go through this, so
// neither can give the key to another kind of thread from the client side.
export function transcriptionSetting(
  kind: string | undefined,
  s: TranscriptionSettings,
): { transcription?: ChatTranscriptionSettings } {
  if (kind !== 'transcription') return {};
  const transcription: ChatTranscriptionSettings = {};
  const language = s.language.trim();
  if (language) transcription.language = language;
  const prompt = s.prompt.trim();
  if (prompt) transcription.prompt = prompt;
  const temperature = toImageNumber(s.temperature);
  if (temperature !== undefined) transcription.temperature = temperature;
  const passthrough = parseTranscriptionPassthrough(s.passthrough);
  if (passthrough.status === 'ok') transcription.passthrough = passthrough.value;
  return { transcription };
}

// Coerce a persisted (all-optional) ChatTranscriptionSettings into the concrete store state,
// filling anything missing/mistyped from the defaults. The inverse of transcriptionSetting,
// used to seed the composer when a chat is loaded: the passthrough object comes back as
// pretty-printed JSON text for the textarea.
export function transcriptionSettingsFrom(
  transcription: ChatTranscriptionSettings | undefined,
): TranscriptionSettings {
  const s = transcription ?? {};
  const passthrough =
    isJsonObject(s.passthrough) && Object.keys(s.passthrough).length > 0
      ? JSON.stringify(s.passthrough, null, 2)
      : DEFAULT_TRANSCRIPTION_SETTINGS.passthrough;
  return {
    language: typeof s.language === 'string' ? s.language : DEFAULT_TRANSCRIPTION_SETTINGS.language,
    prompt: typeof s.prompt === 'string' ? s.prompt : DEFAULT_TRANSCRIPTION_SETTINGS.prompt,
    temperature: imageNumToStr(s.temperature),
    passthrough,
  };
}

// The built-in voice system prompt: what a Voice Chat run carries after the thread's own system
// prompt (as the voice half of its one system message) when the thread stores no voice prompt of
// its own. A COPY of the backend's defaultVoiceSystemPrompt
// (gateway/backend/internal/portal/service_chats.go): the two MUST stay identical, character for
// character (a test reads the Go source and compares). The portal needs the text to pre-fill the
// editable field and to tell an edit from an untouched default -- only an edit is stored, so the
// default stays code-owned and an improved one reaches every thread that did not override it.
export const DEFAULT_VOICE_SYSTEM_PROMPT =
  'You are in a spoken conversation. Summarize the essentials as short spoken ' +
  'sections, each wrapped in its own `<speak lang="…">…</speak>` tag, using the ' +
  'language the user is speaking (set `lang` to its two-letter code). For a ' +
  'short answer use ONE section; for a long answer use several — roughly one per ' +
  'key point — each a brief spoken summary of that part, so the spoken sections ' +
  'together are a fuller but still concise summary. Keep each `<speak>` short ' +
  'enough to listen to — a sentence or two. Write the fuller detail ' +
  '(explanations, lists, code, caveats) as normal text outside the tags; that ' +
  'text is shown but not spoken. Do not put anything before the first `<speak>` ' +
  'tag.';

// The Voice Chat picks of a thread (#317, #318, #314), as CONCRETE store state (every field
// defined, unlike the all-optional wire ChatVoiceChatSettings): the live-transcription model that
// hears the user, the streaming speech model that answers aloud, that speech model's voice, and
// the system prompt of the conversation's runs. For the three picks "" is "no pick" --
// automatic: the portal then uses the first model, in the catalogue's order, that the gateway says
// streams, and the speech model's own default voice (a preset-clone model, which has none, is
// pre-selected on its first preset at READ time, never written here) -- and the default of all
// three. A pick is a gateway model NAME (a voice a name the model offers, or one that was typed);
// whether a model is still on offer is not this state's to say (the store reads it against the
// models the gateway says stream, and falls back to the automatic one when it is gone), so a pick
// that is gone for a while is kept, not erased. The prompt is different in kind: it is never
// "unset" in the state but pre-filled with DEFAULT_VOICE_SYSTEM_PROMPT (what the field shows), and
// is written only when it differs from that. `voiceModel` is the attribution of the voice, not a
// pick of its own: the speech model the saved `voice` was chosen for (#326), kept beside it so the
// reconciliation of a voice against the model in use has a reference across reloads -- a voice is
// a name one model offers, and the model it was chosen for says whether a later change of model
// makes it stale. "" is "not recorded" (and is always the case when there is no voice).
export type VoiceChatSettings = {
  asrModel: string;
  ttsModel: string;
  voice: string;
  voiceModel: string;
  systemPrompt: string;
};

export const DEFAULT_VOICE_CHAT_SETTINGS: VoiceChatSettings = {
  asrModel: '',
  ttsModel: '',
  voice: '',
  voiceModel: '',
  systemPrompt: DEFAULT_VOICE_SYSTEM_PROMPT,
};

// Whether a thread is a TEXT thread, the only one a Voice Chat runs on (the pin is "" -- the
// settings omit it -- for text): what the picks are written for.
function isTextKind(kind: string | undefined): boolean {
  return kind === undefined || kind === '';
}

// The `voice_chat` entry of a settings object, as a SPREADABLE fragment in the style of audioSetting,
// with one difference of kind: it is present ONLY on a TEXT thread (a conversation is a text
// thread's, the speech settings `audio` are a speech thread's and the backend clears them for
// every other kind), and ONLY when a pick is made -- so a thread that never chose stays
// byte-identical, and the key is never an empty `voice_chat: {}`. A pick is trimmed, and a blank
// one is no pick. The system prompt is trimmed too (the backend does the same), and is written
// only when it is non-empty and differs from DEFAULT_VOICE_SYSTEM_PROMPT: a blank or unedited
// prompt writes nothing, and the backend then applies the built-in one. Every member must be in
// this fragment: buildDoc full-replaces `settings` with no merge, so a member left out here is
// erased by the next save. Both the document writer (buildDoc) and the state seeder
// (normalizeDoc) go through this, so neither can give the key to another kind of thread from the
// client side.
export function voiceChatSetting(
  kind: string | undefined,
  s: VoiceChatSettings,
): { voice_chat?: ChatVoiceChatSettings } {
  if (!isTextKind(kind)) return {};
  const voiceChat: ChatVoiceChatSettings = {};
  const asr = s.asrModel.trim();
  if (asr) voiceChat.asr_model = asr;
  const tts = s.ttsModel.trim();
  if (tts) voiceChat.tts_model = tts;
  const voice = s.voice.trim();
  if (voice) voiceChat.voice = voice;
  // The voice's model attribution means something only next to a voice: a model with no voice
  // (or a voice that was never attributed) writes no `voice_model`.
  const voiceModel = s.voiceModel.trim();
  if (voice && voiceModel) voiceChat.voice_model = voiceModel;
  const systemPrompt = s.systemPrompt.trim();
  if (systemPrompt && systemPrompt !== DEFAULT_VOICE_SYSTEM_PROMPT) {
    voiceChat.system_prompt = systemPrompt;
  }
  return Object.keys(voiceChat).length > 0 ? { voice_chat: voiceChat } : {};
}

// Coerce a persisted (all-optional) ChatVoiceChatSettings into the concrete store state, a
// mistyped pick being no pick, and an absent, blank or mistyped prompt the built-in one (what the
// backend runs with in those cases). The inverse of voiceChatSetting, used to seed the thread's
// picks when a chat is loaded.
export function voiceChatSettingsFrom(
  voiceChat: ChatVoiceChatSettings | undefined,
): VoiceChatSettings {
  const s: ChatVoiceChatSettings = isJsonObject(voiceChat) ? voiceChat : {};
  return {
    asrModel: typeof s.asr_model === 'string' ? s.asr_model : DEFAULT_VOICE_CHAT_SETTINGS.asrModel,
    ttsModel: typeof s.tts_model === 'string' ? s.tts_model : DEFAULT_VOICE_CHAT_SETTINGS.ttsModel,
    voice: typeof s.voice === 'string' ? s.voice : DEFAULT_VOICE_CHAT_SETTINGS.voice,
    voiceModel:
      typeof s.voice_model === 'string' ? s.voice_model : DEFAULT_VOICE_CHAT_SETTINGS.voiceModel,
    systemPrompt:
      typeof s.system_prompt === 'string' && s.system_prompt.trim() !== ''
        ? s.system_prompt
        : DEFAULT_VOICE_CHAT_SETTINGS.systemPrompt,
  };
}

// The picker's `accept` list: any audio, plus the extensions a file dialog matches when it has no
// MIME type to go by (IMAGE_EDIT_SOURCE_ACCEPT's twin). A clip is converted to WAV in the browser
// before it is attached (shared/audioToWav), so whatever the browser can decode is welcome, not a
// fixed list of formats; the extensions keep a file the platform does not type as audio/* (an
// audio-only .webm is video/webm in Chrome, an .opus or .flac may have no type at all) from being
// greyed out in the dialog.
export const TRANSCRIPTION_AUDIO_ACCEPT = [
  'audio/*',
  '.wav',
  '.webm',
  '.mp3',
  '.m4a',
  '.ogg',
  '.oga',
  '.opus',
  '.flac',
  '.aac',
].join(',');

// The bare media type of a clip: its `type/subtype` without parameters, lower-cased and
// trimmed, which is the only spelling the backend takes (DecodeAudioDataURL refuses a data
// url whose type carries a parameter). A browser reports a recording or a picked file with
// whatever its platform says (audio/webm;codecs=opus, audio/x-m4a, application/ogg ...), so a
// type is read through here before it is judged or becomes a data url.
export function bareAudioMediaType(mediaType: string): string {
  return mediaType.split(';', 1)[0].trim().toLowerCase();
}

// The inline data url of a clip: `data:<type>;base64,<bytes>`, the type reduced to its bare
// spelling (bareAudioMediaType), which is what the backend reads. `base64` is the clip's bytes,
// standard-alphabet base64 without the data-url prefix. The composer's clip is always the WAV it
// converted the upload to (audioAttach passes audio/wav), whatever the upload was.
export function audioDataUrl(mediaType: string, base64: string): string {
  return `data:${bareAudioMediaType(mediaType)};base64,${base64}`;
}

// The content of the user turn a transcription composer sends: the clip alone, as one audio_url
// part (`url` is the clip's data url, see audioDataUrl). There is no text part: the turn IS the
// audio, and nothing else of it is read.
export function audioTurnContent(url: string): ChatContent {
  return [{ type: 'audio_url', audio_url: { url } }];
}

// Coerce an opaque `content` blob into a well-formed active-chat document,
// applying sane defaults for anything missing/mistyped.
export function normalizeDoc(content: unknown): ActiveChatDoc {
  const doc = (content ?? {}) as { settings?: Partial<ChatSettings>; messages?: unknown };
  const s = doc.settings ?? {};
  const messages = Array.isArray(doc.messages) ? (doc.messages as ChatUiMessage[]) : [];
  // The thread's kind, which every kind-keyed fragment below keys off.
  const kind = typeof s.kind === 'string' ? s.kind : DEFAULTS.kind;
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
      ...kindSetting(kind),
      // The image knobs, round-tripped for an image thread for the same reason
      // as kind: buildDoc rebuilds `settings` from the state this seeds, and the
      // PUT full-replaces the blob with no merge, so a knob dropped here is one
      // the next autosave erases. imageSetting keys off the kind, so a text
      // thread stays byte-identical (no image key).
      ...imageSetting(kind, imageSettingsFrom(s.image)),
      // The audio knobs, round-tripped for a speech thread for the same reason as the
      // image ones above (the PUT full-replaces the blob). audioSetting keys off the
      // kind, so a text or image thread stays byte-identical (no audio key).
      ...audioSetting(kind, audioSettingsFrom(s.audio)),
      // The transcription knobs, round-tripped for a transcription thread for the same
      // reason (the PUT full-replaces the blob). transcriptionSetting keys off the kind, so
      // a text, image or speech thread stays byte-identical (no transcription key).
      ...transcriptionSetting(kind, transcriptionSettingsFrom(s.transcription)),
      // The Voice Chat picks and system prompt, round-tripped for a text thread for the same
      // reason (the PUT full-replaces the blob). voiceChatSetting keys off the kind and writes
      // only a pick that is made and a prompt that is not the default, so a text thread that
      // never chose, and every other kind of thread, stay byte-identical (no voice_chat key).
      ...voiceChatSetting(kind, voiceChatSettingsFrom(s.voice_chat)),
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

// The title of a transcription chat, from its transcript: the latest assistant turn's text, as
// deriveTitle shortens it ('' when there is none). A transcription thread's first user turn is
// audio only, so deriveTitleFromMessages (the first user message's text) finds nothing there;
// the transcript is what names the chat, once a run has produced one.
export function deriveTitleFromTranscript(messages: ChatUiMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i].role === 'assistant') return deriveTitle(textOf(messages[i].content));
  }
  return '';
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

// Whether the LAST user message of a replayed history carries an image -- which,
// in an image thread, is what makes the replayed turn an EDIT (the server's
// executor sends exactly that message's images as the source and reads nothing
// else of the history). Role-aware on purpose, unlike historyHasImage: an image
// thread's assistant turns hold its own generated results, which are never sent.
//
// A SOURCE image: the mask part (maskPart) is an image too, but a message with a
// mask and no source is no edit -- the executor ignores such a mask -- so it must
// not be taken for the operand the replay guard protects.
export function lastUserHasImage(messages: ChatUiMessage[]): boolean {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const { role, content } = messages[i];
    if (role === 'user') {
      return (
        Array.isArray(content) && content.some((p) => p.type === 'image_url' && !isMaskPart(p))
      );
    }
  }
  return false;
}

// Whether the LAST user message of a replayed history carries an audio clip -- which is what a
// transcription replay transcribes (the executor reads exactly that message's audio and nothing
// else of the history). Role-aware like lastUserHasImage: only a user turn's clip is an input.
// A stored /api/portal/chat-blobs/{id} reference counts as one, as it is what a committed turn
// holds.
export function lastUserHasAudio(messages: ChatUiMessage[]): boolean {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const { role, content } = messages[i];
    if (role === 'user') {
      return Array.isArray(content) && content.some((p) => p.type === 'audio_url');
    }
  }
  return false;
}

// The prompt text of the LAST user message of a replayed history ('' when there is
// none). What an image edit is asked to do is this text, and the endpoint refuses an
// edit without it (images.prompt_required) only after the turn was stored.
export function lastUserText(messages: ChatUiMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    if (messages[i].role === 'user') return textOf(messages[i].content);
  }
  return '';
}

let idSeq = 0;
export function nextId(): string {
  const cryptoObj = globalThis.crypto as Crypto | undefined;
  if (cryptoObj?.randomUUID) return cryptoObj.randomUUID();
  idSeq += 1;
  return `msg-${idSeq}`;
}
