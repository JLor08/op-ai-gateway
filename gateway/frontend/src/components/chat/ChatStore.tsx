// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type ReactNode,
  type SetStateAction,
} from 'react';
import type {
  ActiveChatRun,
  Chat,
  ChatSummary,
  ImageModelOptions,
  ModelOption,
  PortalApi,
  PortalServer,
  PortalToken,
  ServerModelOption,
} from '../../api';
import type { Translation } from '../shared/types';
import { formatPortalError } from '../shared/format';
import { useToast } from '../shared/ToastProvider';
import { imageUrlsOf, type ChatContent } from '../shared/chatContent';
import {
  prepareImageDataUrl,
  prepareMaskDataUrl,
  ImageAttachError,
  isHeicFile,
} from '../shared/imageAttach';
import { offeredFlavors } from '../shared/offeredFlavors';
import {
  ACTIVE_ID_KEY,
  DEFAULTS,
  MAX_IMAGES,
  attachModeOf,
  audioPassthroughMessage,
  audioSetting,
  audioSettingsFrom,
  audioTurnContent,
  byNewest,
  clearLegacyKeys,
  DEFAULT_AUDIO_SETTINGS,
  DEFAULT_IMAGE_SETTINGS,
  DEFAULT_TRANSCRIPTION_SETTINGS,
  IMAGE_EDIT_SOURCE_TYPES,
  deriveTitle,
  deriveTitleFromMessages,
  deriveTitleFromTranscript,
  historyHasImage,
  imageSetting,
  imageSettingsFrom,
  isChatBlobUrl,
  lastUserHasAudio,
  lastUserHasImage,
  lastUserText,
  lsGet,
  lsSet,
  nextId,
  normalizeDoc,
  parseAudioPassthrough,
  parseTranscriptionPassthrough,
  readLegacyDoc,
  resolveTokenOverride,
  speechPassthroughInvalid,
  summaryOf,
  transcriptionPassthroughInvalid,
  transcriptionPassthroughMessage,
  transcriptionSetting,
  transcriptionSettingsFrom,
  userTurnContent,
  type AttachMode,
  type AudioSettings,
  type ChatUiMessage,
  type ImageSettings,
  type TranscriptionSettings,
} from './chatDoc';
import { AudioAttachError, prepareAudioClip, type AudioClip } from './audioAttach';
import { useChatRuns } from './useChatRuns';
import { useChatPersistence } from './useChatPersistence';

// Re-exported so existing/external imports of these types from ChatStore keep
// working unchanged after the FA-2 extraction into chatDoc.ts.
export type { ActiveChatDoc, ChatMessageStatus } from './chatDoc';
export type { AttachMode, ChatUiMessage };
// The transcription composer's clip, for the view that previews it.
export type { AudioClip };

// The speech composer's advanced-passthrough check and the settings type it edits,
// re-exported for Chat.tsx for the same reason as the image presets below: the
// composer must not reach into chatDoc.ts (arch.test.ts), and it needs to name a
// passthrough text that is not a JSON object before the user sends it.
export { audioPassthroughMessage, parseAudioPassthrough } from './chatDoc';
export type { AudioPassthroughParse, AudioSettings } from './chatDoc';

// The transcription composer's twin of the above (its own reserved fields and message), and
// what its audio input needs: the allow-listed clip formats for the attach picker and the bare
// media-type helper. (The clip itself is built inside the store, by handleFiles / attachRecording,
// which strip a recorder's media-type parameter and map a platform's alias to the canonical type.)
export {
  bareAudioMediaType,
  parseTranscriptionPassthrough,
  TRANSCRIPTION_AUDIO_ACCEPT,
  TRANSCRIPTION_AUDIO_TYPES,
  transcriptionPassthroughMessage,
} from './chatDoc';
export type { TranscriptionSettings } from './chatDoc';

// The image-settings presets (and the optionsOr helper that merges a per-model
// fetched list over them) the composer (Chat.tsx, OUTSIDE components/chat/)
// renders. Re-exported through this file because the chat module's only public
// surface is ChatStore.tsx / ChatSidebar.tsx -- Chat.tsx must not reach into
// chatDoc.ts directly (arch.test.ts enforces this).
export {
  IMAGE_EDIT_SOURCE_ACCEPT,
  IMAGE_LOSSY_FORMATS,
  IMAGE_N_MAX,
  IMAGE_OUTPUT_FORMATS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  IMAGE_SIZE_OPTIONS,
  optionsOr,
} from './chatDoc';

// The per-message actions the transcript's rows are handed (handlersFor): the user
// turn's edit, the assistant turn's regenerate, and -- on a generated image -- the
// refine that makes it the source of the next edit.
type MessageHandlers = {
  onEdit: (text: string) => void;
  onRegenerate: () => void;
  onRefine: (url: string) => void;
};

export type ChatStore = {
  messages: ChatUiMessage[];
  // The ACTIVE chat's streaming flag (derived from runningChatIds). Kept as a
  // simple boolean so existing consumers (Chat.tsx / ChatSidebar.tsx) are
  // untouched by the multi-run rework.
  streaming: boolean;
  // Chats with a live server run (any chat, not just the active one). Drives
  // the sidebar's per-chat running indicator (Task 3.6). Always supplied by
  // the provider.
  runningChatIds: Set<string>;
  isChatRunning: (id: string) => boolean;
  input: string;
  images: string[];
  // The transcription composer's clip -- the audio the next send transcribes -- or null. It
  // exists only on a transcription thread (handleFiles takes audio there, and it is dropped when
  // the thread is anything else) and is the WHOLE turn: a transcription send needs it and sends
  // nothing else. `url` is the inline data url the turn will carry, already under its canonical
  // media type (what the preview plays too); `name` is the file's, '' for a recording.
  audio: AudioClip | null;
  model: string;
  systemPrompt: string;
  temperature: number;
  maxTokens: number;
  selectedTokenId: string;
  chatModels: ModelOption[];
  modelOptions: ModelOption[];
  usableTokens: PortalToken[];
  // The caller's manageable servers (see ChatStoreProvider's `servers` prop):
  // the server-override picker in the chat settings panel renders only when
  // this is non-empty, and its options come from it.
  manageableServers: PortalServer[];
  serverOverride: string;
  serverOverrideForceUnreachable: boolean;
  setServerOverride: Dispatch<SetStateAction<string>>;
  setServerOverrideForceUnreachable: Dispatch<SetStateAction<boolean>>;
  // True when the selected run-as token carries its OWN server override: the
  // chat's own server-override controls must be locked (disabled) and display
  // the TOKEN's server/force values instead (see effectiveServerOverride/
  // effectiveServerOverrideForce below) -- the backend's precedence is
  // token-first, so the chat's own picked server would not take effect anyway.
  serverOverrideLocksChat: boolean;
  // The server override actually in effect for the next run: the run-as
  // token's value while serverOverrideLocksChat is true, else the chat's own
  // `serverOverride`. Render this (not `serverOverride`) wherever the EFFECTIVE
  // value matters (the picker's displayed value, the model-dropdown filter).
  effectiveServerOverride: string;
  // Likewise the force-unreachable flag actually in effect.
  effectiveServerOverrideForce: boolean;
  // The offered models of the currently-selected (effective) override server
  // (empty while no override is in effect). Once an override is in effect,
  // feed the chat's model dropdown from THIS list instead of modelOptions.
  serverOverrideModels: ServerModelOption[];
  overrideModel: string;
  // True only for a pure catch-all override (a run-as token with a catch-all and
  // no per-model map entries): the model picker is locked to the forced model.
  // With per-model entries the picker stays editable (the pick is the map key).
  overrideLocksModel: boolean;
  // Whether the effective model (overrideModel || model) is currently among
  // the reachable chatModels. False while it's temporarily unreachable — the
  // model stays selected/visible (see modelOptions) but sending is blocked.
  modelAvailable: boolean;
  // Whether the effective model can process image attachments as INPUT of a text
  // turn. One of the flags attachMode (below) is derived from; the attach gate
  // itself (button disabled, send blocked, images cleared on switch) reads
  // attachMode, not this.
  modelVisionCapable: boolean;
  // Whether the effective model GENERATES images. Independent of
  // modelVisionCapable, which is whether it ACCEPTS them. Gates only what the
  // composer NEWLY OFFERS (the image-prompt affordances on an unpinned
  // thread) and feeds attachMode -- never what an existing thread DOES: that is
  // chatKind's job.
  modelImageCapable: boolean;
  // Whether the effective model may be the target of an image EDIT: the backend's
  // derived, operator-removable `image_edit` (ModelOption.image_edit, folded like
  // `image`), read as it is and never recomputed here -- the routing gate reads the
  // same derivation, so the composer never offers an edit the gate would refuse.
  // Independent of modelVisionCapable (an sd model edits and is vision:false).
  modelImageEdit: boolean;
  // Whether the effective model SYNTHESIZES speech (the backend's `speech` verdict,
  // ModelOption.speech, read as served). The speech twin of modelImageCapable: it gates
  // only what the composer NEWLY OFFERS on an unpinned thread (the speech composer, via
  // prospectiveKind) -- never what an existing thread DOES: that is chatKind's job.
  modelSpeechCapable: boolean;
  // Whether the effective model TRANSCRIBES speech (the backend's `transcription` verdict,
  // ModelOption.transcription, read as served). The speech flag's twin: it gates only what the
  // composer NEWLY OFFERS on an unpinned thread (the transcription composer, via
  // prospectiveKind) -- never what an existing thread DOES: that is chatKind's job.
  modelTranscriptionCapable: boolean;
  // What an attached image MEANS for the next send: 'edit' (the source of an image
  // edit), 'vision' (an input a text model reads) or 'none' (not accepted). One
  // predicate of chatKind and the model's flags (attachModeOf, chatDoc.ts) that
  // the attach control, the clear on a model switch, send() and the replay guard
  // all read. See ADR-043 (a), R1.
  attachMode: AttachMode;
  // The per-model sd-server image options (samplers / schedulers / output
  // formats / size limits / batch ceiling / defaults) of the effective model,
  // fetched on demand while that model GENERATES images (api.modelImageOptions).
  // null whenever there is nothing to narrow by: not an image model, the fetch
  // is in flight, or it failed. Every field of a fetched value is optional too
  // ("unknown"), so the composer's dropdowns merge it over their static
  // Phase 1b-i superset (optionsOr) rather than trusting it to be complete.
  modelImageOptions: ImageModelOptions | null;
  // The THREAD's kind: "" (text), "image", "speech" or "transcription". Pinned by the backend
  // at the thread's first send and authoritative from then on; while the thread is
  // still empty it follows the picked model, because that is what the next
  // send will pin it to. Render composer affordances off THIS, not off
  // modelImageCapable — a text thread stays a text thread even if the user
  // later picks an image, speech or transcription model.
  chatKind: string;
  // The image-generation knobs of the active thread (size / n / output_format /
  // output_compression), as concrete store state. The settings panel edits them
  // when chatKind === "image"; they are omitted from the wire and persistence
  // for a text thread (see imageSetting in chatDoc.ts).
  imageSettings: ImageSettings;
  setImageSettings: Dispatch<SetStateAction<ImageSettings>>;
  // The text-to-speech knobs of the active thread (voice / speed / passthrough text), as
  // concrete store state. The speech composer edits them when chatKind === "speech";
  // they are omitted from the wire and persistence for any other kind (see audioSetting
  // in chatDoc.ts). A passthrough that is not a JSON object blocks the send (see send()).
  audioSettings: AudioSettings;
  setAudioSettings: Dispatch<SetStateAction<AudioSettings>>;
  // The speech-to-text knobs of the active thread (language / prompt / temperature /
  // passthrough text), as concrete store state: the audio settings' twin. The transcription
  // composer edits them when chatKind === "transcription"; they are omitted from the wire and
  // persistence for any other kind (see transcriptionSetting in chatDoc.ts). A passthrough
  // that is not a JSON object, or that names a field the run owns, blocks the send (see send()).
  transcriptionSettings: TranscriptionSettings;
  setTranscriptionSettings: Dispatch<SetStateAction<TranscriptionSettings>>;
  // The active chat's LIVE RUN's own reported kind ("" text | "image" | "speech" |
  // "transcription"), read from useChatRuns' kindOf -- deliberately NOT chatKind above.
  // chatKind is the client's own "is this thread's first send" guess
  // (messagesRef.current.length === 0) and can disagree with the server's
  // (len(doc.Messages) > 0): a second tab whose transcript view is stale
  // after another tab's first send would still guess from the picked model
  // and could pin the wrong kind locally, and a client whose optimistic pin
  // survived a rolled-back send carries the same staleness. PrepareChatRun
  // forces the thread's real, pinned kind server-side on every send after
  // the first, and the run's own reports (the 201 / an active-runs entry /
  // an SSE snapshot) are that authoritative, post-force value -- exactly
  // what the executor is actually running. Undefined only when the active
  // chat has no run-registry entry at all.
  //
  // Chat.tsx passes this to ChatMessage ONLY for the row that is actually
  // streaming right now, the same gate as the `streaming` prop itself:
  // passing it to every row unconditionally would hand every ChatMessage a
  // prop that changes with `streaming`'s own churn and defeat its memo for
  // rows that are not the one in flight.
  runKind: string | undefined;
  // The active chat's live run elapsed ms, anchored on the run's own last
  // server-reported measurement and interpolated to "now" at render time
  // (see useChatRuns' elapsedMsOf). Undefined only when the active chat has
  // NO run-registry entry at all (never run, or forgotten) -- a run that has
  // gone terminal but is still lingering in the registry (the eviction
  // grace) returns its frozen final value here, not undefined. Gated into
  // ChatMessage exactly like runKind above, and for the same reason: this
  // value is recomputed on every render and would otherwise be a "different
  // float essentially every time", defeating ChatMessage's memo for the
  // WHOLE transcript on every token delta of any live run, not just the row
  // in flight.
  runElapsedMs: number | undefined;
  // Multi-chat layer.
  chats: ChatSummary[];
  activeChatId: string | null;
  chatsLoading: boolean;
  selectChat: (id: string) => void;
  deleteChat: (id: string) => void;
  renameChat: (id: string, title: string) => void;
  send: () => void;
  stop: () => void;
  newChat: () => void;
  setInput: Dispatch<SetStateAction<string>>;
  // Attach what the composer's attach control picked: on a transcription thread the first file
  // is the audio clip (replacing the one it had), anywhere else the images of a text or an
  // image-edit turn. Refusals (a format, a size) are toasts.
  handleFiles: (fileList: FileList | null) => Promise<void>;
  removeImage: (index: number) => void;
  // Attach a browser recording (the in-browser recorder's blob, typed as the recorder reports it:
  // audio/webm;codecs=opus) as the transcription turn's clip, by the same path and refusals as a
  // picked file. removeAudio drops the clip again.
  attachRecording: (blob: Blob) => Promise<void>;
  removeAudio: () => void;
  // The mask of an image EDIT (inpainting): one PNG data url, or null. It exists
  // only beside a source image in edit mode (handleMaskFile refuses it otherwise,
  // and it is dropped with its source or when the mode changes) and is sent as a
  // part of the turn tagged as a mask (chatDoc.ts maskPart). Distinct from `images`,
  // which are the sources.
  mask: string | null;
  handleMaskFile: (fileList: FileList | null) => Promise<void>;
  removeMask: () => void;
  // A counter the composer watches: it moves each time a refine seeds the composer,
  // which is the view's cue to put the cursor in the prompt field (the user's next
  // move is the instruction, and the composer may be far below the image they clicked).
  // Only its CHANGE means anything; it is 0 until the first refine.
  composerFocusRequest: number;
  setModel: Dispatch<SetStateAction<string>>;
  setSystemPrompt: Dispatch<SetStateAction<string>>;
  setTemperature: Dispatch<SetStateAction<number>>;
  setMaxTokens: Dispatch<SetStateAction<number>>;
  setSelectedTokenId: Dispatch<SetStateAction<string>>;
  handlersFor: (id: string) => MessageHandlers;
};

// The user-facing sentence for a failed attachment: the reason an image could not be
// prepared (type, size, or a decode failure -- or anything else, which is generic).
function attachErrorText(err: unknown, t: Translation): string {
  if (err instanceof ImageAttachError) {
    switch (err.reason) {
      case 'type':
        return t.chatImageErrorType;
      case 'size':
        return t.chatImageErrorSize;
      default:
        // "decode" (or any future reason) -> generic
        return t.chatImageError;
    }
  }
  return t.chatImageError;
}

// The user-facing sentence for a refused audio clip: its format, its size, an empty file --
// or anything else, which is generic.
function audioAttachErrorText(err: unknown, t: Translation): string {
  if (err instanceof AudioAttachError) {
    switch (err.reason) {
      case 'type':
        return t.chatAudioErrorType;
      case 'size':
        return t.chatAudioErrorSize;
      case 'empty':
        return t.chatAudioErrorEmpty;
      default:
        // "read" (or any future reason) -> generic
        return t.chatAudioError;
    }
  }
  return t.chatAudioError;
}

// Exported so tests can inject a store value directly (e.g. the Chat view)
// without spinning up the full stream driver.
export const ChatStoreContext = createContext<ChatStore | null>(null);

// The streaming flag lives in its OWN context with a primitive value, kept
// separate from the full store on purpose: the store `value` is a fresh object
// on every provider render (once per streamed token), so a consumer reading it
// would re-render per token. `useChatStreaming` reads this boolean context
// instead, so a per-token-only consumer like NavSidebar re-renders only when
// streaming actually flips. Default false = safe outside any provider.
export const ChatStreamingContext = createContext<boolean>(false);

export function useChatStore(): ChatStore {
  const c = useContext(ChatStoreContext);
  if (!c) throw new Error('useChatStore must be used within ChatStoreProvider');
  return c;
}

// Safe outside a provider (e.g. NavSidebar renders whether or not the chat store
// is mounted): no provider -> false. Subscribes ONLY to the streaming boolean,
// not the per-token store value (see ChatStreamingContext above).
export function useChatStreaming(): boolean {
  return useContext(ChatStreamingContext);
}

export function ChatStoreProvider({
  api,
  models,
  tokens,
  servers,
  onRefresh,
  refreshModels,
  t,
  children,
}: Readonly<{
  api: Pick<
    PortalApi,
    | 'activeChatRuns'
    | 'cancelChatRun'
    | 'chat'
    | 'chats'
    | 'createChat'
    | 'deleteChat'
    | 'modelImageOptions'
    | 'saveChat'
    | 'saveChatKeepalive'
    | 'serverModels'
    | 'startChatRun'
  >;
  models: ModelOption[];
  tokens: PortalToken[];
  // The caller's MANAGEABLE servers (api.servers() -> ListServers, which is
  // already manager-scoped server-side). Drives whether the chat's
  // server-override picker renders at all (hidden with zero manageable
  // servers) and its option list. Optional so pre-existing test renders that
  // never touch the picker keep working unchanged; defaults to [].
  servers?: PortalServer[];
  // Called after every stream ends (reloads tokens/models). Accepts a sync
  // fire-and-forget refresh (App passes the SILENT loader) or an async one; the
  // store awaits it either way.
  onRefresh: () => void | Promise<void>;
  // Lightweight models-only refresh (no token/dashboard/server reload), polled
  // while the effective model is unavailable so the "!" clears automatically
  // once the upstream recovers. Optional so existing test renders that don't
  // care about recovery keep working unchanged.
  refreshModels?: () => void;
  t: Translation;
  children: ReactNode;
}>) {
  // The active chat's live state. Content is loaded from the server (defaults
  // until the first chat is activated); the legacy localStorage seed is handled
  // in the one-time migration effect below, not here.
  const [model, setModel] = useState<string>(DEFAULTS.model);
  const [systemPrompt, setSystemPrompt] = useState<string>(DEFAULTS.system_prompt);
  const [temperature, setTemperature] = useState<number>(DEFAULTS.temperature);
  const [maxTokens, setMaxTokens] = useState<number>(DEFAULTS.max_tokens);
  // The active image thread's generation knobs (concrete, see ImageSettings).
  // Seeded from the loaded document in activateChat, sent on run via
  // currentSettings, persisted via buildDoc -- all gated on chatKind "image".
  const [imageSettings, setImageSettings] = useState<ImageSettings>(DEFAULT_IMAGE_SETTINGS);
  // The active speech thread's text-to-speech knobs (concrete, see AudioSettings): the
  // image settings' twin, seeded in activateChat, sent via currentSettings, persisted
  // via buildDoc -- all gated on chatKind "speech".
  const [audioSettings, setAudioSettings] = useState<AudioSettings>(DEFAULT_AUDIO_SETTINGS);
  // The active transcription thread's speech-to-text knobs (concrete, see
  // TranscriptionSettings): the audio settings' twin, seeded in activateChat, sent via
  // currentSettings, persisted via buildDoc -- all gated on chatKind "transcription".
  const [transcriptionSettings, setTranscriptionSettings] = useState<TranscriptionSettings>(
    DEFAULT_TRANSCRIPTION_SETTINGS,
  );
  const [messages, setMessages] = useState<ChatUiMessage[]>([]);
  const [input, setInput] = useState('');
  const [images, setImages] = useState<string[]>([]);
  // The transcription composer's clip (see ChatStore.audio).
  const [audio, setAudio] = useState<AudioClip | null>(null);
  const [mask, setMask] = useState<string | null>(null);
  const [composerFocusRequest, setComposerFocusRequest] = useState(0);
  const [selectedTokenId, setSelectedTokenId] = useState<string>(DEFAULTS.run_as_token_id);
  const [serverOverride, setServerOverride] = useState<string>(DEFAULTS.server_override);
  const [serverOverrideForceUnreachable, setServerOverrideForceUnreachable] = useState<boolean>(
    DEFAULTS.server_override_force_unreachable,
  );
  // The distinct gateway models the selected override server offers (see
  // api.serverModels), used to narrow the chat's model dropdown once a server
  // override is picked. Empty while no override is set.
  const [serverOverrideModels, setServerOverrideModels] = useState<ServerModelOption[]>([]);
  // The effective image model's per-model sd-server options (see api.modelImageOptions),
  // used to narrow the composer's image dropdowns. null while there is none.
  const [modelImageOptions, setModelImageOptions] = useState<ImageModelOptions | null>(null);

  // The caller's manageable servers (undefined prop -> []): drives whether the
  // server-override picker renders at all and its option list.
  const manageableServers = useMemo(() => servers ?? [], [servers]);

  // The THREAD's pinned kind as last loaded/established ("" text | "image" | "speech" |
  // "transcription").
  // Real state, not a value read once at load: the pin is established by the
  // thread's FIRST send, and a live session (new chat -> pick an image model
  // -> send -> send again -> regenerate, no reload) must see it immediately.
  // Opaque string, never narrowed to the kinds this build knows, so an
  // unfamiliar one survives the save round trip instead of being erased. ""
  // is text: the SETTING is optional (omitted for a text thread, mirroring
  // the backend's omitempty) but the state is always a string, so nothing
  // downstream has to handle undefined.
  const [pinnedChatKind, setPinnedChatKind] = useState<string>('');

  // The chat list + which one is active.
  const [chats, setChats] = useState<ChatSummary[]>([]);
  const [activeChatId, setActiveChatId] = useState<string | null>(null);
  const [chatsLoading, setChatsLoading] = useState(true);

  const { showError } = useToast();

  // A model is offered when the chat can send it something the gateway will
  // serve: a text request (chat completions, the openai flavor) or an image
  // request (the openai_images flavor) or a speech or transcription request (the
  // openai_audio flavor). offeredFlavors keeps openai_images only behind an image
  // verdict, because the images gate refuses a model without image: yes -- an
  // images-only model (an sd-server, whose upstream has no chat endpoint) without
  // that verdict could only ever produce a request that fails. openai_audio is the
  // same story and is admitted only behind a speech or a transcription verdict
  // (the two endpoints are both served under that one flavor, and each gate
  // refuses a model without its own verdict), but that rule is the CHAT's alone
  // and lives here: offeredFlavors passes openai_audio through because it is
  // shared with the Models view, whose "Available via" column must keep showing
  // an audio row's flavor (unprobed ones and agent-managed ones whose agent has
  // not reported either verdict have none and are listed there anyway).
  // anthropic alone offers nothing: the chat sends no /v1/messages request.
  const chatModels = useMemo(
    () =>
      models.filter((option) => {
        const offered = offeredFlavors(option);
        return (
          offered.includes('openai') ||
          offered.includes('openai_images') ||
          (offered.includes('openai_audio') &&
            (option.speech === true || option.transcription === true))
        );
      }),
    [models],
  );
  // The dropdown's option list. A non-empty `model` that is not (or no longer)
  // in chatModels is injected as a synthetic trailing option so SearchableSelect
  // keeps showing + selecting it (a value not in `options` renders blank) —
  // this is what lets an unavailable saved model stay visible instead of
  // disappearing. Falls back to a single fabricated option when chatModels is
  // empty (e.g. before the list has loaded, or everything is unreachable), same
  // as before, but now also covers a saved model in that fallback.
  const modelOptions: ModelOption[] = useMemo(() => {
    const savedMissing = model !== '' && !chatModels.some((option) => option.id === model);
    // Keep a saved-but-currently-missing model visible (injected) so it stays
    // selected while unreachable; otherwise just the real models. An empty
    // selection injects nothing (the field is intentionally clearable/searchable).
    if (savedMissing)
      return [
        ...chatModels,
        { id: model, display_name: model, flavors: ['openai'], loading_on_count: 0 },
      ];
    return chatModels;
  }, [chatModels, model]);

  // Only the caller's own active gateway:use tokens may be selected as a
  // run-as target (mirrors the backend's AuthorizeRunAsToken check).
  const usableTokens = useMemo(
    () => tokens.filter((tk) => tk.status === 'active' && tk.scopes.includes('gateway:use')),
    [tokens],
  );
  const selectedToken = usableTokens.find((tk) => tk.id === selectedTokenId);
  // The run-as token's override for the CURRENTLY-selected model: an exact
  // per-model map entry wins, otherwise the catch-all (model_override), otherwise
  // none. Mirrors the backend's resolveModelOverride so the effective model shown
  // matches what the server will route (the server does the actual remap via the
  // run-as token; the chat sends the raw picked model, see currentSettings).
  const overrideModel = resolveTokenOverride(selectedToken, model);
  // A pure catch-all (a catch-all with no per-model entries) forces every model, so
  // the model picker is locked to it (old single-override behavior). With per-model
  // entries the picked model IS the map key, so the picker stays editable.
  const overrideMapHasEntries =
    !!selectedToken?.model_override_map && Object.keys(selectedToken.model_override_map).length > 0;
  const overrideLocksModel =
    !!selectedToken && !overrideMapHasEntries && (selectedToken.model_override ?? '') !== '';

  // Whether the run-as token carries its OWN server override: the backend's
  // precedence is token-first (a chat-header override loses to the token's
  // stored one — see applyServerOverride), so when the token has one, the
  // chat's own server-override controls are locked to display the token's
  // values instead of the chat's own picked server, mirroring overrideLocksModel
  // above. The chat's own `serverOverride`/`serverOverrideForceUnreachable`
  // state is left untouched while locked (it is what the picker reverts to once
  // the token is cleared).
  const serverOverrideLocksChat = !!selectedToken && (selectedToken.server_override ?? '') !== '';
  // The server override actually in effect for the next run: the token's value
  // while locked, else the chat's own picked server.
  const effectiveServerOverride = serverOverrideLocksChat
    ? (selectedToken?.server_override ?? '')
    : serverOverride;
  const effectiveServerOverrideForce = serverOverrideLocksChat
    ? !!selectedToken?.server_override_force_unreachable
    : serverOverrideForceUnreachable;

  // The model actually used for the next run (an active run-as override wins
  // over the chat's own `model`), and whether it currently appears in the
  // reachable-models list. Drives the red "!" indicator and the send guard.
  const effectiveModel = overrideModel || model;
  const modelAvailable =
    effectiveModel !== '' && chatModels.some((option) => option.id === effectiveModel);
  // Whether the effective model can process images (drives the chat image gate).
  // A model/group is vision-capable only when ALL its mappings/members are (the
  // backend already AND-aggregates this into ModelOption.vision).
  const modelVisionCapable =
    chatModels.find((option) => option.id === effectiveModel)?.vision ?? false;
  // Whether the effective model GENERATES images (drives the image-thread
  // composer). Independent of modelVisionCapable, which is whether it ACCEPTS
  // them; the backend AND-aggregates both into ModelOption.
  const modelImageCapable =
    chatModels.find((option) => option.id === effectiveModel)?.image ?? false;
  // Whether the effective model may be EDITED (ModelOption.image_edit, derived in
  // the backend from `image` unless an operator opted out). Read as served: the
  // gate and this flag are one derivation (routing.ImpliedImageEdit).
  const modelImageEdit =
    chatModels.find((option) => option.id === effectiveModel)?.image_edit ?? false;
  // Whether the effective model SYNTHESIZES speech (drives the speech-thread composer),
  // read as served like `image`: the backend's own fold of the speech verdict.
  const modelSpeechCapable =
    chatModels.find((option) => option.id === effectiveModel)?.speech ?? false;
  // Whether the effective model TRANSCRIBES speech (drives the transcription-thread composer),
  // read as served like `speech`: the backend's own fold of the transcription verdict.
  const modelTranscriptionCapable =
    chatModels.find((option) => option.id === effectiveModel)?.transcription ?? false;

  // The kind the NEXT send would pin, derived from the picked model. Only
  // ever consulted while the thread has no transcript at all (below), which
  // is why the synthetic model option injected for a picked-but-uncatalogued
  // model — no capability fields, so image === false — is harmless here: the
  // PINNED kind is the thread's truth the moment one exists.
  //
  // The precedence is image -> speech -> transcription, each kind the later one never
  // displaces: a model both image- and speech-capable is an image model here (image was the
  // kind first, so a model that gains a speech verdict never changes what an existing image
  // model does), and one both speech- and transcription-capable is a speech model. Nothing in
  // the catalogue carries two verdicts today: the audio.cpp probe gives an asr model a
  // transcription verdict only and a tts model a speech one only, so two arise solely from
  // manual configuration, and then the later mode is simply unreachable for that one model.
  let prospectiveKind = '';
  if (modelImageCapable) prospectiveKind = 'image';
  else if (modelSpeechCapable) prospectiveKind = 'speech';
  else if (modelTranscriptionCapable) prospectiveKind = 'transcription';
  // The thread's kind, and the whole distinction in one line: once the thread
  // has a transcript the backend has already pinned it (PrepareChatRun forces
  // the stored kind on every send after the first), so the pin wins and the
  // currently-picked model is irrelevant; while it is still empty there is no
  // pin yet, so the composer shows what the next send would establish.
  const chatKind = messages.length > 0 ? pinnedChatKind : prospectiveKind;
  // What an attached image means for the next send (see attachModeOf): derived
  // from the thread's kind, so an attachment follows the THREAD, never merely the
  // picked model.
  const attachMode = attachModeOf(chatKind, {
    vision: modelVisionCapable,
    image: modelImageCapable,
    imageEdit: modelImageEdit,
  });

  // Refs mirror the latest state so the stable callbacks below (send / edit /
  // regenerate / save) can read current values without being re-created every
  // render.
  const messagesRef = useRef(messages);
  const modelRef = useRef(model);
  const systemPromptRef = useRef(systemPrompt);
  const temperatureRef = useRef(temperature);
  const maxTokensRef = useRef(maxTokens);
  const imageSettingsRef = useRef(imageSettings);
  const audioSettingsRef = useRef(audioSettings);
  const transcriptionSettingsRef = useRef(transcriptionSettings);
  const selectedTokenIdRef = useRef(selectedTokenId);
  const serverOverrideRef = useRef(serverOverride);
  const serverOverrideForceUnreachableRef = useRef(serverOverrideForceUnreachable);

  const chatsRef = useRef(chats);
  const activeChatIdRef = useRef(activeChatId);
  // The active chat's current title (used by saves + auto-title); not React
  // state because it is only read in callbacks, never rendered directly.
  const activeTitleRef = useRef<string>('');
  // Monotonic token for async chat loads (selectChat). Each load captures the
  // current value; when it resolves it only activates if it is still the latest
  // request, so rapid clicks can't let a slow fetch win out of order.
  const loadReqRef = useRef(0);

  // api / onRefresh / t are read through refs so the stream + save callbacks
  // stay stable and always see the latest values (a background stream may
  // finish after a locale change or a fresh loadPortalData binding).
  const apiRef = useRef(api);
  apiRef.current = api;
  const onRefreshRef = useRef(onRefresh);
  onRefreshRef.current = onRefresh;
  const refreshModelsRef = useRef(refreshModels);
  refreshModelsRef.current = refreshModels;
  const tRef = useRef(t);
  tRef.current = t;
  const showErrorRef = useRef(showError);
  showErrorRef.current = showError;
  // Mirror the derived availability so the stable send/edit/regenerate callbacks
  // (which read via refs, not props) can gate on the latest value.
  const modelAvailableRef = useRef(modelAvailable);
  modelAvailableRef.current = modelAvailable;
  // Mirror the derived vision-capability so the stable send/edit/regenerate
  // callbacks (which read via refs, not props) can gate on the latest value.
  // Direct render-time assignment (not an effect) to match modelAvailableRef
  // above — an effect would lag by one passive-effect flush.
  const modelVisionCapableRef = useRef(modelVisionCapable);
  modelVisionCapableRef.current = modelVisionCapable;
  // Likewise the attach predicate and the one capability its refusal wording needs
  // (an image GENERATOR is told it takes no input, any other model that it cannot
  // read one).
  const modelImageCapableRef = useRef(modelImageCapable);
  modelImageCapableRef.current = modelImageCapable;
  const attachModeRef = useRef<AttachMode>(attachMode);
  attachModeRef.current = attachMode;
  // Mirror the thread's kind so the stable send/edit/regenerate callbacks and
  // buildDoc (which read via refs, not props) always see the latest value.
  // Direct render-time assignment, like modelAvailableRef/modelVisionCapableRef
  // above — an effect would lag by one passive-effect flush, and this ref used
  // to be written only in activateChat, which made it permanently stale for
  // every thread pinned during the current session.
  const chatKindRef = useRef<string>(chatKind);
  chatKindRef.current = chatKind;

  // The run/SSE engine (FA-2): owns the per-chat run subscriptions + transcript
  // buffers behind a narrow interface. See useChatRuns.ts.
  const runs = useChatRuns(
    apiRef,
    activeChatIdRef,
    messagesRef,
    setMessages,
    onRefreshRef,
    showErrorRef,
    tRef,
  );
  const {
    runningChatIds,
    isRunning,
    statusOf: runStatusOf,
    runIdIfRunning,
    kindOf,
    elapsedMsOf,
    buffers: chatBuffers,
    registerRunning,
    subscribe: subscribeRun,
    forget: forgetRun,
    closeAll: closeAllRuns,
    onTerminal,
    onTranscriptStale,
  } = runs;

  // The active chat's streaming flag. A run is bound to a chat, not to "the UI",
  // so streaming reflects only the chat currently on screen; background runs
  // still progress and keep their own entry in runningChatIds.
  const streaming = activeChatId ? runningChatIds.has(activeChatId) : false;

  // The active chat's LIVE RUN's own reported kind and elapsed ms (both
  // undefined with no registry entry at all), recomputed at every render.
  // Deliberately NOT chatKind: chatKind is the client's own "is this thread's
  // first send" guess and can be stale relative to the server's view (a
  // second tab still holding messages = [] after another tab's first send,
  // or a client whose optimistic pin survived a rolled-back send) --
  // PrepareChatRun forces the thread's real pinned kind server-side on every
  // send after the first, and runKind is read from that authoritative,
  // post-force value (the 201 / an active-runs entry / an SSE snapshot),
  // never re-derived from settings. `streaming` above cannot be true for
  // this chat before kindOf/elapsedMsOf are already populated for it --
  // markRunning(true) is called only from inside the same openStream() call
  // that seeds them (useChatRuns.ts) -- so gating a render on `streaming` and
  // reading `runKind` together is safe with no window where they disagree.
  // ChatMessage's own ImagePendingTurn does the per-second ticking locally
  // (an image run emits no incremental events to re-render THIS on), so
  // runElapsedMs need not tick here.
  const runKind = activeChatId ? kindOf(activeChatId) : undefined;
  const runElapsedMs = activeChatId ? elapsedMsOf(activeChatId) : undefined;

  // The persistence layer (FA-2): owns buildDoc/flushSave, the debounced-save
  // effect, the pagehide keepalive, and the unmount flush behind a narrow
  // interface. See useChatPersistence.ts.
  const persistence = useChatPersistence(
    {
      activeChatIdRef,
      messagesRef,
      modelRef,
      systemPromptRef,
      temperatureRef,
      maxTokensRef,
      imageSettingsRef,
      audioSettingsRef,
      transcriptionSettingsRef,
      selectedTokenIdRef,
      serverOverrideRef,
      serverOverrideForceUnreachableRef,
      chatKindRef,
      activeTitleRef,
      apiRef,
      showErrorRef,
      tRef,
    },
    {
      activeChatId,
      messages,
      model,
      systemPrompt,
      temperature,
      maxTokens,
      imageSettings,
      audioSettings,
      transcriptionSettings,
      selectedTokenId,
      serverOverride,
      serverOverrideForceUnreachable,
      kind: chatKind,
    },
    isRunning,
    setChats,
  );
  const {
    flushSave,
    clearDirty,
    skipNextSave,
    cancelPendingSave,
    setTranscriptStale,
    flushOnUnmount,
  } = persistence;

  // Wire persistence's dirty-clearing into the run engine's terminal callback
  // (replaces a direct dirtyRef write formerly inline in finishRun). Cheap ref
  // write on every render, matching this file's existing "read latest via a
  // stable ref, assigned at render time" style (see apiRef/showErrorRef above).
  onTerminal((chatId) => {
    if (chatId === activeChatIdRef.current) clearDirty();
    // Names an untitled transcription chat after its transcript. Defined below, with the
    // renameChat it goes through; this callback only ever RUNS after the render is done.
    titleFromTranscript(chatId);
  });
  // ...and persistence's save suppression into the run engine's canonical
  // refetch. NOT gated on the active chat: the engine adopts background chats
  // too, and activateChat prefers a chat's streamed buffer over the freshly
  // loaded doc, so an unproven background buffer would otherwise become the
  // active transcript and be PUT over the server's good copy.
  onTranscriptStale(setTranscriptStale);

  useEffect(() => {
    messagesRef.current = messages;
    modelRef.current = model;
    systemPromptRef.current = systemPrompt;
    temperatureRef.current = temperature;
    maxTokensRef.current = maxTokens;
    imageSettingsRef.current = imageSettings;
    audioSettingsRef.current = audioSettings;
    transcriptionSettingsRef.current = transcriptionSettings;
    selectedTokenIdRef.current = selectedTokenId;
    serverOverrideRef.current = serverOverride;
    serverOverrideForceUnreachableRef.current = serverOverrideForceUnreachable;
    chatsRef.current = chats;
    activeChatIdRef.current = activeChatId;
  });

  // Cache per-message callbacks so memoized ChatMessage children keep stable
  // prop references across re-renders.
  const handlerCacheRef = useRef(new Map<string, MessageHandlers>());

  // Load a chat's content into the active state. Calls persistence's
  // skipNextSave so the resulting state changes do not trigger an immediate
  // re-save.
  const activateChat = useCallback(
    (chat: Chat) => {
      // Preserve the outgoing active chat's live transcript in its buffer so
      // returning to it shows the latest (esp. a background run still streaming).
      const outgoing = activeChatIdRef.current;
      if (outgoing) chatBuffers.set(outgoing, messagesRef.current);

      const doc = normalizeDoc(chat.content);
      skipNextSave();
      cancelPendingSave();
      setModel(doc.settings.model);
      setSystemPrompt(doc.settings.system_prompt);
      setTemperature(doc.settings.temperature);
      setMaxTokens(doc.settings.max_tokens);
      setSelectedTokenId(doc.settings.run_as_token_id);
      setServerOverride(doc.settings.server_override);
      setServerOverrideForceUnreachable(doc.settings.server_override_force_unreachable);
      // The thread's pinned kind, like every other setting: seeded from the
      // loaded document. A setting activateChat does NOT seed reverts to its
      // default here and is then written over the stored value by the next
      // debounced save, because the PUT full-replaces the content blob.
      setPinnedChatKind(doc.settings.kind ?? '');
      // The image knobs, seeded like every other setting: a thread activateChat
      // does not seed reverts to defaults and is written over the stored value
      // by the next save (the PUT full-replaces the blob). normalizeDoc kept the
      // persisted `image` only for an image thread, so a text thread seeds
      // defaults harmlessly.
      setImageSettings(imageSettingsFrom(doc.settings.image));
      // The audio knobs, seeded the same way and for the same reason (a speech thread
      // activateChat does not seed reverts to defaults and is written over the stored
      // value by the next save). normalizeDoc kept the persisted `audio` only for a
      // speech thread, so any other thread seeds defaults harmlessly.
      setAudioSettings(audioSettingsFrom(doc.settings.audio));
      // The transcription knobs, seeded likewise: normalizeDoc kept the persisted
      // `transcription` only for a transcription thread, so any other thread seeds defaults
      // harmlessly, and one activateChat did not seed would be written over by the next save.
      setTranscriptionSettings(transcriptionSettingsFrom(doc.settings.transcription));
      // Seed the transcript from the per-chat buffer when this chat has an active
      // or recently-finished run (the server owns/owned its tail); otherwise use
      // the freshly-loaded server content. Prefer the buffer ONLY when it actually
      // holds streamed content — never an empty/fabricated buffer over the real
      // server doc. At bootstrap a run's metadata is registered before any
      // snapshot has streamed in, so the buffer is empty/absent and the doc wins.
      const buffered = chatBuffers.get(chat.id);
      const streamed =
        buffered && buffered.length > 0 && runStatusOf(chat.id) !== undefined ? buffered : null;
      let seed = streamed ?? doc.messages;
      // Seeding from `chat.content` is the one moment this client KNOWS its
      // transcript is the server's, so it is where a stale mark left by a
      // failed canonical refetch is lifted. Seeding from the buffer proves
      // nothing and therefore lifts nothing.
      if (!streamed) setTranscriptStale(chat.id, false);
      // Interrupted detection (Task 3.4): a trailing `pending` assistant with no
      // active run was cut off — a gateway restart lost the run from the registry.
      // Keep the partial output but mark it interrupted. A chat WITH an active run
      // is left alone; its subscription's snapshot replaces the tail live.
      if (runStatusOf(chat.id) !== 'running') {
        const last = seed.at(-1);
        if (last?.role === 'assistant' && last.status === 'pending') {
          seed = [...seed.slice(0, -1), { ...last, status: 'interrupted' as const }];
        }
      }
      setMessages(seed);
      messagesRef.current = seed;
      chatBuffers.set(chat.id, seed);
      handlerCacheRef.current.clear();
      // A source seeded by a refine is a reference to a blob of the chat it was
      // refined in, so it does not follow the user to another chat: sent there the run
      // would be refused (images.blob_not_owned). An uploaded image is just bytes and
      // goes where the composer goes, as it always did.
      setImages((prev) => prev.filter((url) => !isChatBlobUrl(url)));
      setActiveChatId(chat.id);
      activeChatIdRef.current = chat.id;
      activeTitleRef.current = chat.title ?? '';
      lsSet(ACTIVE_ID_KEY, chat.id);
    },
    [chatBuffers, runStatusOf, skipNextSave, cancelPendingSave, setTranscriptStale],
  );

  // The current per-chat generation settings sent when starting a run. The model
  // is the RAW picked model; when a run-as token is selected the server applies
  // that token's override map + catch-all (resolveModelOverride) exactly once —
  // pre-mapping here would double-apply (the catch-all would re-fire on an
  // already-mapped model). run_as_token_id carries the token so the server remaps.
  const currentSettings = useCallback(
    () => ({
      model: modelRef.current,
      system_prompt: systemPromptRef.current,
      temperature: temperatureRef.current,
      max_tokens: maxTokensRef.current,
      run_as_token_id: selectedTokenIdRef.current,
      server_override: serverOverrideRef.current,
      server_override_force_unreachable: serverOverrideForceUnreachableRef.current,
      // On the thread's FIRST send this establishes the pin; on every later
      // send the backend ignores it and forces the stored one, so sending the
      // thread's own kind is both correct and a no-op there.
      kind: chatKindRef.current,
      // The image knobs, present only on an image thread (imageSetting keys off
      // the kind). The backend forwards the set fields to the images endpoint.
      ...imageSetting(chatKindRef.current, imageSettingsRef.current),
      // The audio knobs, present only on a speech thread (audioSetting keys off the
      // kind), so a text or image thread's run body carries no audio key. The backend
      // forwards the set fields to /v1/audio/speech.
      ...audioSetting(chatKindRef.current, audioSettingsRef.current),
      // The transcription knobs, present only on a transcription thread
      // (transcriptionSetting keys off the kind), so a text, image or speech thread's run
      // body carries no transcription key. The backend forwards the set fields as form fields
      // of /v1/audio/transcriptions.
      ...transcriptionSetting(chatKindRef.current, transcriptionSettingsRef.current),
    }),
    [],
  );

  // buildDoc/flushSave, the debounced-save effect, and the pagehide keepalive
  // now live in useChatPersistence.ts (FA-2); `persistence` above exposes the
  // narrow interface this provider needs.

  // Flush immediately when a stream finishes (streaming true -> false): the
  // per-token writes were skipped, so this captures the final transcript (and
  // any auto-title) even if the user has navigated away.
  const prevStreamingRef = useRef(streaming);
  useEffect(() => {
    const was = prevStreamingRef.current;
    prevStreamingRef.current = streaming;
    if (was && !streaming) void flushSave();
  }, [streaming, flushSave]);

  // One-time bootstrap: load the chat list, migrate any legacy conversation,
  // and open a chat (there is always exactly one active chat afterwards). All
  // best-effort — a failed/empty fetch must not crash the app.
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const list = await apiRef.current.chats();
        if (cancelled) return;
        const data = Array.isArray(list?.data) ? list.data : [];
        // Fetch the still-running server runs (Task 3.4) and REGISTER their
        // metadata into the run engine WITHOUT opening an EventSource yet.
        // Registering first keeps the opened chat's interrupted-detection
        // seeing the active run so it does not falsely mark a live pending
        // tail interrupted. Deferring the EventSource until AFTER the active
        // chat is loaded ensures no run snapshot is ever applied onto a
        // not-yet-loaded chat (which would fabricate a lone-message buffer and
        // then be trusted over the real server doc).
        let activeRuns: ActiveChatRun[] = [];
        try {
          const active = await apiRef.current.activeChatRuns();
          if (cancelled) return;
          activeRuns = active.data;
          for (const run of activeRuns)
            registerRunning(run.chat_id, run.run_id, run.kind, run.elapsed_ms);
        } catch {
          /* best-effort: no active-run replay */
        }
        if (data.length > 0) {
          setChats(byNewest(data));
          const storedId = lsGet(ACTIVE_ID_KEY);
          const target =
            (storedId && data.find((chat) => chat.id === storedId)) || byNewest(data)[0];
          const full = await apiRef.current.chat(target.id);
          if (cancelled) return;
          activateChat(full);
        } else {
          // Empty list: migrate a legacy conversation if present, else start fresh.
          const legacy = readLegacyDoc();
          const created = legacy
            ? await apiRef.current.createChat({
                title: deriveTitleFromMessages(legacy.messages),
                content: legacy,
              })
            : await apiRef.current.createChat({
                title: '',
                content: { settings: DEFAULTS, messages: [] },
              });
          clearLegacyKeys();
          if (cancelled) return;
          setChats([summaryOf(created)]);
          activateChat(created);
        }
        // Now open the EventSources. The active chat's transcript is loaded, so
        // its snapshot updates the already-shown trailing assistant; a background
        // running chat is seeded from its server doc inside subscribeRun before
        // its snapshot is applied.
        for (const run of activeRuns)
          subscribeRun(run.chat_id, run.run_id, run.kind, run.elapsed_ms);
      } catch (err) {
        if (!cancelled) showErrorRef.current(formatPortalError(err, tRef.current));
      } finally {
        if (!cancelled) setChatsLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activateChat]);

  // The provider is mounted app-wide (above the view switch), so navigation no
  // longer unmounts it — in-flight runs keep generating server-side. This effect
  // fires ONLY on a real provider unmount (logout — a client-side state change,
  // NOT a page reload), never on navigation. Close every open EventSource (the
  // server run keeps going and is re-subscribed on next load) and flush a final
  // save for the active chat unless it is mid-run (server owns that transcript).
  useEffect(
    () => () => {
      closeAllRuns();
      flushOnUnmount();
    },
    [closeAllRuns, flushOnUnmount],
  );

  // No auto-selection of a model: an empty selection is a valid state the user can
  // return to (clear the field to search freely). A fresh chat therefore starts
  // with NO model — the user picks one, and Send stays disabled until they do
  // (modelAvailable below). A non-empty saved model also stays selected even when
  // momentarily missing from chatModels (upstream unreachable), shown as
  // unavailable until it returns.

  // While a PICKED model is unavailable, lightly poll the models list so the "!"
  // clears and Send re-enables automatically once it becomes reachable again, with
  // no user action needed. Skipped when nothing is selected (effectiveModel === "")
  // — there is no model to wait for — and stops as soon as it's available.
  useEffect(() => {
    if (modelAvailable || effectiveModel === '') return;
    const id = setInterval(() => {
      refreshModelsRef.current?.();
    }, 15000);
    return () => clearInterval(id);
  }, [modelAvailable, effectiveModel]);

  // Clear a composer attachment the (newly) effective model will not use —
  // e.g. switching from a vision-capable model to one that is not, with an
  // image still attached. Runs whenever the attach predicate changes (it is
  // derived from the thread's kind and the model's flags, see attachModeOf); a
  // no-op while there is nothing attached.
  //
  // An attachment is cleared only when the new mode accepts NONE. Moving between
  // 'vision' and 'edit' (an empty thread's kind follows the picked model, so
  // picking an image model after attaching to a vision one) keeps it: it is the
  // user's operand either way, and the server decides what it is from the thread.
  // What a mode cannot take it refuses where it is used (send(), handleFiles).
  //
  // The image-GENERATOR case is not the same refusal as a plain non-vision
  // model: such a model may well be vision-capable too, and a generator that
  // cannot be edited (an operator's opt-out) takes no input at all -- an image
  // run without a source is a generation, so the attachment would be dropped in
  // silence. It says so in its own words.
  useEffect(() => {
    if (images.length === 0 || attachMode !== 'none') return;
    setImages([]);
    showError(modelImageCapable ? t.chatImageGeneratorNoInput : t.chatImageModelUnsupported);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [attachMode]);

  // A mask is the mask OF an edit's source image, so it cannot outlive either: it
  // goes when the source is removed (or sent) and when the attach mode is no longer
  // 'edit' -- a text model reading the same image has no use for one. Nothing needs
  // to be said: the strip it was in shows it gone, and the source's own clearing
  // (above) already speaks when a model takes no image at all.
  useEffect(() => {
    if (mask === null) return;
    if (attachMode !== 'edit' || images.length === 0) setMask(null);
  }, [mask, attachMode, images.length]);

  // A clip belongs to the transcription composer: once the thread is anything else (a different
  // model picked on an empty thread, another chat opened) there is no control that shows or
  // removes it, and it would otherwise sit unseen in memory -- up to 20 MB of it -- ready to
  // reappear in a later transcription composer. So it goes with the composer, without a word:
  // there is nothing on screen left to say it about.
  useEffect(() => {
    if (audio !== null && chatKind !== 'transcription') setAudio(null);
  }, [audio, chatKind]);

  // Reset a stale run-as selection: onRefresh reloads `tokens` after every
  // stream, and a chat may persist a run-as token that was later
  // deleted/disabled/expired or lost gateway:use — it would otherwise keep
  // being sent as X-OP-Run-As-Token (backend 403) and leave the native
  // <select value={...}> uncontrolled. Fall back to the session ("") default.
  useEffect(() => {
    if (selectedTokenId !== '' && !usableTokens.some((tk) => tk.id === selectedTokenId)) {
      setSelectedTokenId('');
    }
  }, [usableTokens, selectedTokenId]);

  // Mirror the run-as reset above for a stale server override: `servers`
  // reloads after every stream (onRefresh), and the picked server may have
  // been deleted or lost from the caller's manageable set since it was
  // selected. The server ALSO self-heals this on every run (PrepareChatRun),
  // but resetting here too keeps the picker's own display honest instead of
  // silently rendering blank (a value absent from its options).
  useEffect(() => {
    if (serverOverride !== '' && !manageableServers.some((s) => s.id === serverOverride)) {
      setServerOverride('');
      setServerOverrideForceUnreachable(false);
    }
  }, [manageableServers, serverOverride]);

  // Load the EFFECTIVE override server's offered models so the chat's model
  // dropdown can narrow to them (Task 6, step 3; keyed on the effective value
  // per Task 2 so a token-locked server also filters the dropdown). Latest-wins
  // guard so a slow/stale response for a previously-selected server can't
  // clobber a newer selection; an empty override clears the list without a
  // fetch.
  useEffect(() => {
    if (effectiveServerOverride === '') {
      setServerOverrideModels([]);
      return;
    }
    let cancelled = false;
    void apiRef.current
      .serverModels(effectiveServerOverride)
      .then((rows) => {
        if (!cancelled) setServerOverrideModels(rows);
      })
      .catch(() => {
        if (!cancelled) setServerOverrideModels([]);
      });
    return () => {
      cancelled = true;
    };
  }, [effectiveServerOverride]);

  // Load the effective IMAGE model's per-model sd-server options so the
  // composer's image dropdowns can narrow to what that model actually supports.
  // Same shape as the serverOverrideModels effect above: latest-wins guard so a
  // slow/stale response for a previously-picked model can't clobber a newer
  // pick, a non-image (or no) model clears without a fetch, and a failure
  // clears too -- null makes the dropdowns fall back to the static superset.
  // Unlike that effect it also clears on every model change (see below).
  // Keyed on the picked model, not the thread's kind: an existing text thread
  // that picks an image model still fetches, which is harmless because the
  // composer only renders these controls for an image thread.
  useEffect(() => {
    // Cleared up front, not only on the non-image branch: these options belong
    // to ONE model, so the previous model's must not stay exposed (and
    // selectable) while the next model's fetch is in flight.
    setModelImageOptions(null);
    if (!modelImageCapable || effectiveModel === '') return;
    let cancelled = false;
    void apiRef.current
      .modelImageOptions(effectiveModel)
      .then((options) => {
        if (!cancelled) setModelImageOptions(options);
      })
      .catch(() => {
        if (!cancelled) setModelImageOptions(null);
      });
    return () => {
      cancelled = true;
    };
  }, [modelImageCapable, effectiveModel]);

  // The run/SSE engine's subscribe/finishRun/adoptCanonicalTranscript/
  // ensureAssistant/writeAssistant/updateChatMessages/markRunning now live in
  // useChatRuns.ts (FA-2); `runs` above exposes the narrow interface this
  // provider needs (subscribeRun, isRunning, chatBuffers, onTerminal, ...).

  // The refusal for a run of this thread whose advanced passthrough cannot be sent, as the
  // sentence to show ('' when it can): a speech or transcription thread whose passthrough text
  // is not a JSON object (the setting would omit it, and the user's extra fields would vanish
  // from the request in silence, with the Advanced section that holds them perhaps collapsed)
  // or names a field the run owns (the run would commit the turn and then fail it
  // server-side). Read by send() and by the replay entry points, which send the thread's
  // CURRENT settings too; only the thread's own kind has a passthrough, so another kind's
  // stale text never blocks anything.
  const passthroughRefusal = useCallback((): string => {
    const kind = chatKindRef.current;
    if (speechPassthroughInvalid(kind, audioSettingsRef.current)) {
      return audioPassthroughMessage(
        parseAudioPassthrough(audioSettingsRef.current.passthrough),
        tRef.current,
      );
    }
    if (transcriptionPassthroughInvalid(kind, transcriptionSettingsRef.current)) {
      return transcriptionPassthroughMessage(
        parseTranscriptionPassthrough(transcriptionSettingsRef.current.passthrough),
        tRef.current,
      );
    }
    return '';
  }, []);

  // Start a run whose message history REPLACES the chat's transcript (edit /
  // regenerate). The optimistic history is shown immediately; the server run
  // then generates a fresh assistant turn appended after it.
  const startRunWithHistory = useCallback(
    async (chatId: string, history: ChatUiMessage[]) => {
      // Block edit/regenerate while the effective model is unavailable — BEFORE
      // any state mutation. Otherwise the setMessages(history) below would
      // truncate the transcript (dropping the assistant turn) and the debounced
      // save would PUT that truncation, permanently losing data, all for a run
      // that is doomed anyway. Mirrors send()'s guard for these entry points.
      if (!modelAvailableRef.current) {
        showErrorRef.current(tRef.current.chatModelUnavailable);
        return;
      }
      // Same guard as send()'s for a speech or transcription thread's advanced
      // passthrough: a replay sends the thread's CURRENT settings too, and one that is not a
      // JSON object would be dropped from the request in silence, and one that names a field
      // the run owns would fail the replay after the turn is committed.
      const refusal = passthroughRefusal();
      if (refusal) {
        showErrorRef.current(refusal);
        return;
      }
      // A transcription replay transcribes the LAST user turn's clip and reads nothing else
      // of the history, so a turn without one has nothing to transcribe -- a stray text turn
      // (an edit of it keeps only its text), or one whose clip an earlier edit dropped. The
      // run would be started after the transcript was truncated and the endpoint would
      // refuse it with nothing to say about why; refused here instead, with the transcript
      // untouched. Judged on the REPLAYED history, like the image guard below: replaying an
      // earlier turn drops every later clip.
      if (chatKindRef.current === 'transcription' && !lastUserHasAudio(history)) {
        showErrorRef.current(tRef.current.chatTranscriptionReplayNoAudio);
        return;
      }
      // Same guard for image support: the REPLAYED history (what is actually
      // sent as edited_history below) must not carry an image the model can't
      // process. Checking `history` (not the full messagesRef.current) matters
      // when regenerating/editing an EARLIER turn: truncation drops everything
      // after it, so a later turn's image is never resent and must not block.
      //
      // It is deliberately role-BLIND for a text thread: buildAPIHistory
      // forwards every message's content verbatim, so an assistant's own
      // generated image in a replayed history becomes a vision input just as a
      // user's upload does, and refusing it on a non-vision model is correct.
      //
      // An IMAGE thread's request carries no history: the executor reads the
      // LAST user message and nothing else, so an older image -- an earlier
      // edit's source, any generated result -- is never sent and has nothing to
      // refuse. Without that exemption an image thread refused its own
      // regenerate from the second turn onward, telling the user that a model
      // whose only purpose is images does not support images. The exemption
      // stops exactly at the last user message, though: when IT carries an image
      // the replayed turn is an EDIT (ADR-043 R1), and an edit needs a model the
      // gateway will serve one for -- the same predicate that offered the attach
      // control. Refused here, before any state is touched, rather than started
      // and answered by the endpoint's own refusal after the transcript was
      // truncated.
      //
      // An edit also needs its prompt, the same rule the composer holds at send():
      // a replay whose last user message has an image but no text (an edit that
      // blanks the prompt, or a turn the old composer stored without one) would be
      // truncated, started, and then refused by the endpoint's images.prompt_required.
      if (chatKindRef.current === 'image') {
        if (lastUserHasImage(history)) {
          if (attachModeRef.current !== 'edit') {
            showErrorRef.current(
              modelImageCapableRef.current
                ? tRef.current.chatImageGeneratorNoInput
                : tRef.current.chatImageModelUnsupported,
            );
            return;
          }
          if (lastUserText(history).trim() === '') {
            showErrorRef.current(tRef.current.chatImageEditPromptRequired);
            return;
          }
        }
      } else if (!modelVisionCapableRef.current && historyHasImage(history)) {
        showErrorRef.current(tRef.current.chatImageModelUnsupported);
        return;
      }
      setMessages(history);
      messagesRef.current = history;
      chatBuffers.set(chatId, history);
      handlerCacheRef.current.clear();
      try {
        const res = await apiRef.current.startChatRun(chatId, {
          edited_history: history.map((m) => ({ ...m })),
          settings: currentSettings(),
        });
        subscribeRun(chatId, res.run_id, res.kind, res.elapsed_ms);
      } catch (err) {
        showErrorRef.current(formatPortalError(err, tRef.current));
      }
    },
    [subscribeRun, currentSettings, chatBuffers, passthroughRefusal],
  );

  // Set the active chat's title from the first user message, when it is still
  // untitled. Updates the sidebar immediately; persisted on the next save.
  const maybeAutoTitle = useCallback((text: string) => {
    const id = activeChatIdRef.current;
    if (!id) return;
    const hasUserMessage = messagesRef.current.some((message) => message.role === 'user');
    if (hasUserMessage) return;
    const current = activeTitleRef.current.trim();
    if (current !== '' && current !== tRef.current.chatNewChat) return;
    const title = deriveTitle(text);
    if (!title) return;
    activeTitleRef.current = title;
    setChats((prev) => prev.map((chat) => (chat.id === id ? { ...chat, title } : chat)));
  }, []);

  const editUserMessage = useCallback(
    (id: string, text: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      if (isRunning(chatId)) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      const current = messagesRef.current;
      const index = current.findIndex((message) => message.id === id);
      if (index < 0) return;
      const original = current[index];
      let content: ChatContent = text;
      if (Array.isArray(original.content)) {
        // Every image part of the turn goes back as it was -- an image thread's edit
        // turn keeps its source, and a masked edit its tagged mask part too (it is an
        // image_url part like the source), in their original order. So does a
        // transcription turn's clip (its audio_url part, which only a USER turn of that kind
        // carries): it is the one thing the run reads, so an edit that dropped it would leave
        // the replayed turn with nothing to transcribe.
        const attachments = original.content.filter(
          (part) => part.type === 'image_url' || part.type === 'audio_url',
        );
        content = [{ type: 'text', text }, ...attachments];
      }
      const edited: ChatUiMessage = {
        ...original,
        content,
        reasoning: undefined,
        reasoningMs: undefined,
        ttftMs: undefined,
        tps: undefined,
      };
      void startRunWithHistory(chatId, [...current.slice(0, index), edited]);
    },
    [startRunWithHistory, isRunning],
  );

  const regenerate = useCallback(
    (id: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      if (isRunning(chatId)) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      const current = messagesRef.current;
      const index = current.findIndex((message) => message.id === id);
      if (index < 0) return;
      const history = current.slice(0, index); // drop this assistant turn + anything after
      if (history.length === 0) return;
      void startRunWithHistory(chatId, history);
    },
    [startRunWithHistory, isRunning],
  );

  // Refine a generated image in place: seed the composer with it as the source of the
  // next edit, BY REFERENCE. The url is the image part's own /api/portal/chat-blobs/{id}
  // (an old chat's inline data url is the one other shape it can have), so the source
  // part of the turn the user sends is that same string -- nothing is fetched, copied
  // or re-embedded, and the transcript never grows by the image's bytes. The run's
  // commit vets the reference like any other (it must be a blob of THIS chat), and the
  // executor resolves it to bytes as it does a stored source (PrepareChatRun).
  //
  // It only SEEDS: an edit is the prompt applied to the image, so the user types the
  // instruction and sends, and send() holds the prompt rule as it does for an upload.
  // Nothing is started or stored here, and no mask is attached for them (the mask slot
  // is theirs, as it is for any source).
  //
  // Same gates as the attach control and the replay guard, from the same predicate: an
  // edit-capable model on an image thread (attachMode 'edit'), and no live run in this
  // chat. The view offers the control only where both hold; this refuses the rest
  // (a stale click, a model switched between render and click) in the same words.
  // Scope is the generated result: an assistant turn's own image, by id and url, so a
  // user turn's image or a url the turn does not hold is a no-op, not a source.
  const refineImage = useCallback(
    (id: string, url: string) => {
      const chatId = activeChatIdRef.current;
      if (!chatId) return;
      const message = messagesRef.current.find((m) => m.id === id);
      if (message?.role !== 'assistant' || !imageUrlsOf(message.content).includes(url)) return;
      if (isRunning(chatId)) {
        showErrorRef.current(tRef.current.chatBusy);
        return;
      }
      if (attachModeRef.current !== 'edit') {
        showErrorRef.current(
          modelImageCapableRef.current
            ? tRef.current.chatImageGeneratorNoInput
            : tRef.current.chatImageModelUnsupported,
        );
        return;
      }
      // An edit has ONE source, so the refined image takes the place of whatever was
      // attached -- and of the mask drawn for that: it would not fit this image.
      setImages([url]);
      setMask(null);
      setComposerFocusRequest((n) => n + 1);
    },
    [isRunning],
  );

  const handlersFor = useCallback(
    (id: string) => {
      const cache = handlerCacheRef.current;
      let handlers = cache.get(id);
      if (!handlers) {
        handlers = {
          onEdit: (text: string) => editUserMessage(id, text),
          onRegenerate: () => regenerate(id),
          onRefine: (url: string) => refineImage(id, url),
        };
        cache.set(id, handlers);
      }
      return handlers;
    },
    [editUserMessage, regenerate, refineImage],
  );

  // Send the composed message: append the optimistic user bubble, clear the
  // composer, then start a SERVER run and subscribe to it. One run per chat —
  // refuse if this chat already has a live run. On a hard start failure the
  // optimistic user bubble is rolled back.
  async function send() {
    const chatId = activeChatIdRef.current;
    if (!chatId) return;
    if (!modelAvailable) {
      showErrorRef.current(tRef.current.chatModelUnavailable);
      return;
    }
    if (isRunning(chatId)) {
      showErrorRef.current(tRef.current.chatBusy);
      return;
    }
    // A transcription turn is its clip and nothing else: no text stands in for it (the composer
    // has no text field, and whatever was typed before the model was picked is not part of it),
    // and no image reaches it (the thread's attach mode is 'none', which also clears one). So
    // it needs the clip, and ignores the text and the images.
    const transcribing = chatKindRef.current === 'transcription';
    const clip = transcribing ? audio : null;
    if (transcribing && clip === null) {
      showErrorRef.current(tRef.current.chatTranscriptionNeedsAudio);
      return;
    }
    const text = transcribing ? '' : input.trim();
    if (!clip && !text && images.length === 0) return;
    if (!transcribing && images.length > 0) {
      const mode = attachModeRef.current;
      if (mode === 'none') {
        showErrorRef.current(
          modelImageCapableRef.current
            ? tRef.current.chatImageGeneratorNoInput
            : tRef.current.chatImageModelUnsupported,
        );
        return;
      }
      // An edit has one source image. (The picker takes one in this mode; this is
      // the carry-over from a vision model that had several attached. Nothing is
      // dropped: the user removes the extras and sends.)
      if (mode === 'edit' && images.length > 1) {
        showErrorRef.current(tRef.current.chatImageEditSingleSource);
        return;
      }
      // An edit is the prompt applied to the image: without the prompt the endpoint
      // answers images.prompt_required, but only after the turn was stored. Refused
      // here instead, with the composer left as it is (the image stays attached).
      if (mode === 'edit' && !text) {
        showErrorRef.current(tRef.current.chatImageEditPromptRequired);
        return;
      }
    }
    // A speech or transcription thread's advanced passthrough must be a JSON object (or
    // blank) that names none of the fields the run owns: any other text would be dropped
    // from the run in silence (audioSetting / transcriptionSetting omit it) while the
    // Advanced section that holds it may be collapsed, and a reserved field would fail the
    // run only after the turn is committed. Refused here with the composer left as it is,
    // before anything is stored or cleared.
    const refusal = passthroughRefusal();
    if (refusal) {
      showErrorRef.current(refusal);
      return;
    }
    // This send pins the thread's kind server-side when the thread is still
    // empty (PrepareChatRun accepts the client's kind only on a genuine first
    // send). Record the same pin here, in the same batch as the optimistic
    // user bubble: from the next render on, `messages` is non-empty and the
    // thread's kind comes from the pin rather than from the picked model.
    if (messagesRef.current.length === 0) setPinnedChatKind(chatKindRef.current);

    // The text, the attached source and -- for an edit -- the mask, tagged as one; or, on a
    // transcription thread, the clip alone.
    const content = clip
      ? audioTurnContent(clip.url)
      : userTurnContent(text, images, mask, attachModeRef.current);
    maybeAutoTitle(text);
    const userMessage: ChatUiMessage = { id: nextId(), role: 'user', content };
    const next = [...messagesRef.current, userMessage];
    setMessages(next);
    messagesRef.current = next;
    chatBuffers.set(chatId, next);
    setInput('');
    setImages([]);
    setMask(null);
    setAudio(null);

    try {
      const res = await apiRef.current.startChatRun(chatId, {
        user_message: content,
        settings: currentSettings(),
      });
      subscribeRun(chatId, res.run_id, res.kind, res.elapsed_ms);
    } catch (err) {
      showErrorRef.current(formatPortalError(err, tRef.current));
      const rolledBack = messagesRef.current.filter((m) => m.id !== userMessage.id);
      setMessages(rolledBack);
      messagesRef.current = rolledBack;
      chatBuffers.set(chatId, rolledBack);
    }
  }

  // Stop the active chat's run by asking the server to cancel it; the UI updates
  // when the run's `done` (status canceled) event arrives over the stream.
  const stop = useCallback(() => {
    const chatId = activeChatIdRef.current;
    if (!chatId) return;
    const runId = runIdIfRunning(chatId);
    if (runId) void apiRef.current.cancelChatRun(chatId, runId);
  }, [runIdIfRunning]);

  // New chat: flushes the current chat, then creates + activates a fresh empty
  // server chat. Any in-flight run keeps generating in the background (its
  // EventSource stays open). Best-effort — a failed POST still clears locally.
  const newChat = useCallback(() => {
    void (async () => {
      await flushSave();
      handlerCacheRef.current.clear();
      try {
        const created = await apiRef.current.createChat({
          title: '',
          content: { settings: DEFAULTS, messages: [] },
        });
        setChats((prev) =>
          byNewest([summaryOf(created), ...prev.filter((chat) => chat.id !== created.id)]),
        );
        activateChat(created);
      } catch (err) {
        showErrorRef.current(formatPortalError(err, tRef.current));
        skipNextSave();
        setMessages([]);
        messagesRef.current = [];
      }
    })();
  }, [flushSave, activateChat, skipNextSave]);

  // Switch chats: always allowed now (background runs keep progressing via their
  // own EventSource + buffer). Flushes the current chat, then loads the target's
  // content — or seeds from its buffer when it has an active/recent run.
  const selectChat = useCallback(
    (id: string) => {
      if (id === activeChatIdRef.current) return;
      const req = ++loadReqRef.current;
      void (async () => {
        await flushSave();
        try {
          const full = await apiRef.current.chat(id);
          // A later click superseded this load — drop the stale result so the
          // last-clicked chat always wins, regardless of fetch order.
          if (req !== loadReqRef.current) return;
          activateChat(full);
        } catch (err) {
          if (req !== loadReqRef.current) return;
          showErrorRef.current(formatPortalError(err, tRef.current));
        }
      })();
    },
    [flushSave, activateChat],
  );

  const deleteChat = useCallback(
    (id: string) => {
      void (async () => {
        // Close any open run subscription for the deleted chat and drop its
        // per-chat run/buffer bookkeeping (the backend DELETE also cancels the
        // server-side run).
        forgetRun(id);
        // Deleting the active chat: cancel any pending debounced save first so it
        // can't fire a PUT against the row we're about to remove (which would 404
        // and pop a spurious "chat not found" toast just after a successful delete).
        if (id === activeChatIdRef.current) cancelPendingSave();
        try {
          await apiRef.current.deleteChat(id);
        } catch (err) {
          showErrorRef.current(formatPortalError(err, tRef.current));
          return;
        }
        const remaining = chatsRef.current.filter((chat) => chat.id !== id);
        setChats(remaining);
        chatsRef.current = remaining;
        if (id !== activeChatIdRef.current) return;
        // Deleted the active chat: open the next newest, or start fresh.
        try {
          if (remaining.length > 0) {
            const full = await apiRef.current.chat(byNewest(remaining)[0].id);
            activateChat(full);
          } else {
            const created = await apiRef.current.createChat({
              title: '',
              content: { settings: DEFAULTS, messages: [] },
            });
            setChats([summaryOf(created)]);
            activateChat(created);
          }
        } catch (err) {
          showErrorRef.current(formatPortalError(err, tRef.current));
        }
      })();
    },
    [activateChat, forgetRun, cancelPendingSave],
  );

  const renameChat = useCallback((id: string, title: string) => {
    const trimmed = title.trim();
    if (!trimmed) return;
    // Captured BEFORE the optimistic write, so the catch below can put both
    // back. `previousTitle` is undefined only for an id that is not in the
    // list at all, in which case the optimistic map below is a no-op too and
    // there is nothing to roll back.
    //
    // `activeIdAtWrite` is captured too, and it is the load-bearing one: it
    // is what lets the catch tell "the ref still belongs to the chat I wrote
    // it for" from "the user has since moved on", which the ids alone at
    // catch time cannot express. See the catch.
    const previousTitle = chatsRef.current.find((chat) => chat.id === id)?.title;
    const activeIdAtWrite = activeChatIdRef.current;
    const wroteActiveTitle = id === activeIdAtWrite;
    const previousActiveTitle = activeTitleRef.current;
    setChats((prev) => prev.map((chat) => (chat.id === id ? { ...chat, title: trimmed } : chat)));
    if (wroteActiveTitle) activeTitleRef.current = trimmed;
    void (async () => {
      try {
        // The backend's PUT keeps the stored content when the body carries
        // no `content` key, so a rename writes only the title and never
        // depends on -- or risks overwriting with -- the local transcript.
        const saved = await apiRef.current.saveChat(id, { title: trimmed });
        setChats((prev) =>
          byNewest(
            prev.map((chat) =>
              chat.id === id ? { ...chat, title: saved.title, updated_at: saved.updated_at } : chat,
            ),
          ),
        );
      } catch (err) {
        // Roll the optimistic title back. Without this the sidebar keeps
        // showing a name the server rejected until the next reload, when it
        // silently reverts -- a toast saying "that failed" next to a title
        // that looks like it succeeded. The one writer of the four with no
        // client-side run gate, so the 409 that PUT /chats/{id} answers
        // while a run is active reaches it by design.
        //
        // On EVERY failure code, not just 409: a rename refused for any
        // reason (404 on a chat deleted in another tab, a 5xx, ...) leaves
        // the same lie on screen, and "which codes revert" is a distinction
        // nothing downstream could act on.
        //
        // THE REF IS NOT THIS CHAT'S TITLE. `activeTitleRef` is the title
        // the debounced autosave PUTs for whatever chat is active RIGHT
        // NOW, and "right now" is after an await that the user spent doing
        // whatever they liked -- including switching chats. An
        // unconditional restore here therefore writes one chat's old title
        // into another chat's ref, and the next autosave persists the
        // rename the server just refused onto a chat nobody renamed, with
        // no toast about it: rename Alpha, click over to Beta, watch the
        // 409 this rollback exists for, and Beta is saved as "Alpha".
        //
        // So the restore undoes the write above only when the ref still
        // belongs to the chat that write was for: this call wrote it
        // (`wroteActiveTitle`) AND the active chat has not changed since
        // (`activeChatIdRef.current === activeIdAtWrite`). Both conditions
        // are needed, and each rules out a different hazard:
        //
        //   - not written, still same active chat -- a rename of a
        //     NON-active chat. The ref is some other chat's title and was
        //     never ours to touch.
        //   - written, active chat changed -- the Alpha/Beta case above.
        //     `activateChat` already set the ref to the NEW chat's own
        //     title (it writes activeChatIdRef and activeTitleRef in the
        //     same breath), so there is nothing to repair and everything to
        //     break.
        //   - not written, active chat changed TO the renamed one -- the
        //     mirror image: `previousActiveTitle` is the title of a chat
        //     that is no longer active, and `activateChat` has again
        //     already set the ref correctly.
        //
        // The list row (above) has no such problem: it is keyed by id, so
        // it is always safe to put back.
        if (previousTitle !== undefined) {
          setChats((prev) =>
            prev.map((chat) => (chat.id === id ? { ...chat, title: previousTitle } : chat)),
          );
        }
        if (wroteActiveTitle && activeChatIdRef.current === activeIdAtWrite) {
          activeTitleRef.current = previousActiveTitle;
        }
        showErrorRef.current(formatPortalError(err, tRef.current));
      }
    })();
  }, []);

  // Name an untitled TRANSCRIPTION chat after its transcript when a run of it completes (Fork 4
  // of the transcription design). A transcription thread's first user turn is audio only, so
  // maybeAutoTitle (which names a chat from the first message's TEXT, at send) has nothing to
  // use and the chat would stay untitled; the transcript is the natural name, and it exists
  // only now. Wired into the run engine's terminal callback (above), so it fires once per run
  // that reached a terminal state, for ANY chat -- the run's own reported kind and status
  // decide, never the active chat's.
  //
  // Gated on the run's own kind ("transcription": the post-force value the executor ran, not
  // the picker) so a text or speech chat is named as before, on a COMPLETED run (a failed or
  // canceled one has no transcript worth a name), and on the chat having no title yet, the
  // new-chat placeholder counting as none exactly as maybeAutoTitle counts it. An empty
  // transcript names nothing: the chat keeps the sidebar's localized "untitled" placeholder.
  //
  // It goes through renameChat, so the title shows at once and is PERSISTED by a title-only
  // save that never touches the transcript -- which is what keeps it for a chat that is not
  // the active one (the debounced autosave only ever saves the active chat), and for one whose
  // canonical refetch failed (its saves are blocked, see setTranscriptStale).
  const titleFromTranscript = useCallback(
    (chatId: string) => {
      if (kindOf(chatId) !== 'transcription' || runStatusOf(chatId) !== 'completed') return;
      const current = (
        chatId === activeChatIdRef.current
          ? activeTitleRef.current
          : (chatsRef.current.find((chat) => chat.id === chatId)?.title ?? '')
      ).trim();
      if (current !== '' && current !== tRef.current.chatNewChat) return;
      const title = deriveTitleFromTranscript(chatBuffers.get(chatId) ?? []);
      if (title) renameChat(chatId, title);
    },
    [kindOf, runStatusOf, chatBuffers, renameChat],
  );

  const removeImage = useCallback(
    (index: number) => setImages((prev) => prev.filter((_, position) => position !== index)),
    [],
  );

  // Vet and attach one audio clip (a picked file, or a recording with no name) as the
  // transcription turn's clip, replacing the one it had; a refused clip leaves that one as it
  // was and says why. It lands only while the thread is still a transcription one: the picker
  // may have been left behind by a model switch while the file was being read.
  async function attachAudioClip(file: Blob, name: string) {
    try {
      const clip = await prepareAudioClip(file, name);
      if (chatKindRef.current === 'transcription') setAudio(clip);
    } catch (err) {
      showError(audioAttachErrorText(err, t));
    }
  }

  const attachRecording = (blob: Blob) => attachAudioClip(blob, '');
  const removeAudio = useCallback(() => setAudio(null), []);

  async function handleFiles(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return;
    // On a transcription thread the attachment is the audio clip -- the one thing the turn is --
    // not an image: the attach control there offers audio only, and what it picks is the first
    // file (a turn has one clip).
    if (chatKind === 'transcription') {
      await attachAudioClip(fileList[0], fileList[0].name);
      return;
    }
    // An image edit has ONE source image; every other attachment is capped at
    // MAX_IMAGES. The cap follows the attach mode like everything else about the
    // attachment does.
    const limit = attachMode === 'edit' ? 1 : MAX_IMAGES;
    // Local running counter: the closed-over `images.length` is stale across the
    // awaits below, so a multi-file select would never trip the cap mid-batch.
    let count = images.length;
    let lastError = '';
    for (const file of Array.from(fileList)) {
      if (count >= limit) {
        lastError = limit === 1 ? t.chatImageEditSingleSource : t.chatImageErrorCount;
        break;
      }
      // The edit route of sd-server reads png and jpeg only. The picker offers just
      // those (and HEIC/HEIF, which prepareImageDataUrl converts to JPEG, so what is
      // attached -- and sent on -- is a JPEG), but `accept` is a hint a file dialog's
      // "all files" (or a drop) does not honor, so the type is checked here too.
      if (
        attachMode === 'edit' &&
        !IMAGE_EDIT_SOURCE_TYPES.includes(file.type.toLowerCase()) &&
        !isHeicFile(file)
      ) {
        lastError = t.chatImageEditSourceType;
        continue;
      }
      try {
        const url = await prepareImageDataUrl(file);
        setImages((prev) => (prev.length >= limit ? prev : [...prev, url]));
        count += 1;
      } catch (err) {
        lastError = attachErrorText(err, t);
      }
    }
    // Set one trailing error so a multi-file batch shows a single toast rather
    // than flickering per file (and no stale error survives a later success).
    if (lastError) showError(lastError);
  }

  const removeMask = useCallback(() => setMask(null), []);

  // The mask slot of an edit: ONE PNG, and only beside a source image. It is sized to
  // the source as it is actually sent (prepareMaskDataUrl: size cap, decode check, the
  // source's own pixel size), NOT clamped to the 1568 px longest edge an uploaded
  // source is downscaled to. The two sources differ: an upload is downscaled to that
  // cap, but a refined image is sent by reference at its stored, natural size
  // (1792x1024 is a size the composer offers), and the gateway refuses a mask that is
  // not exactly the first source's size -- a clamped mask would never fit a refined
  // image, and the user has no way to make one that does. A PNG stays a PNG, alpha
  // included. The gateway checks the rest (a transparent region to edit, and the size
  // of a mask that was not of the source's shape) and says so with images.mask_invalid.
  async function handleMaskFile(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return;
    if (attachMode !== 'edit' || images.length === 0) {
      showError(t.chatMaskNeedsSource);
      return;
    }
    const file = fileList[0];
    // The mask's alpha is the selection, so only a PNG can carry it (a jpeg has no
    // alpha, a webp or gif would be refused by the gateway). `accept` is a hint, so
    // the type is checked here too.
    if (file.type.toLowerCase() !== 'image/png') {
      showError(t.chatMaskErrorType);
      return;
    }
    try {
      setMask(await prepareMaskDataUrl(file, images[0]));
    } catch (err) {
      showError(attachErrorText(err, t));
    }
  }

  const value: ChatStore = {
    messages,
    streaming,
    runningChatIds,
    isChatRunning: (id: string) => runningChatIds.has(id),
    input,
    images,
    audio,
    model,
    systemPrompt,
    temperature,
    maxTokens,
    selectedTokenId,
    chatModels,
    modelOptions,
    usableTokens,
    manageableServers,
    serverOverride,
    serverOverrideForceUnreachable,
    setServerOverride,
    setServerOverrideForceUnreachable,
    serverOverrideLocksChat,
    effectiveServerOverride,
    effectiveServerOverrideForce,
    serverOverrideModels,
    overrideModel,
    overrideLocksModel,
    modelAvailable,
    modelVisionCapable,
    modelImageCapable,
    modelImageEdit,
    modelSpeechCapable,
    modelTranscriptionCapable,
    attachMode,
    modelImageOptions,
    chatKind,
    imageSettings,
    setImageSettings,
    audioSettings,
    setAudioSettings,
    transcriptionSettings,
    setTranscriptionSettings,
    runKind,
    runElapsedMs,
    chats,
    activeChatId,
    chatsLoading,
    selectChat,
    deleteChat,
    renameChat,
    send,
    stop,
    newChat,
    setInput,
    handleFiles,
    removeImage,
    attachRecording,
    removeAudio,
    mask,
    handleMaskFile,
    removeMask,
    composerFocusRequest,
    setModel,
    setSystemPrompt,
    setTemperature,
    setMaxTokens,
    setSelectedTokenId,
    handlersFor,
  };

  // Streaming boolean in its own (primitive) provider so per-token store-value
  // churn does not re-render streaming-only consumers (NavSidebar).
  return (
    <ChatStreamingContext.Provider value={streaming}>
      <ChatStoreContext.Provider value={value}>{children}</ChatStoreContext.Provider>
    </ChatStreamingContext.Provider>
  );
}
