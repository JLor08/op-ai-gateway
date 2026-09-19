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
  ModelOption,
  PortalApi,
  PortalServer,
  PortalToken,
  ServerModelOption,
} from '../../api';
import type { Translation } from '../shared/types';
import { formatPortalError } from '../shared/format';
import { useToast } from '../shared/ToastProvider';
import type { ChatContent } from '../shared/chatContent';
import { prepareImageDataUrl, ImageAttachError } from '../shared/imageAttach';
import { imagesLeft } from '../shared/chatCapacity';
import {
  ACTIVE_ID_KEY,
  DEFAULTS,
  MAX_IMAGES,
  byNewest,
  clearLegacyKeys,
  deriveTitle,
  deriveTitleFromMessages,
  historyHasImage,
  lsGet,
  lsSet,
  nextId,
  normalizeDoc,
  readLegacyDoc,
  resolveTokenOverride,
  summaryOf,
  type ChatUiMessage,
} from './chatDoc';
import { useChatRuns } from './useChatRuns';
import { useChatPersistence } from './useChatPersistence';

// Re-exported so existing/external imports of these types from ChatStore keep
// working unchanged after the FA-2 extraction into chatDoc.ts.
export type { ActiveChatDoc, ChatMessageStatus } from './chatDoc';
export type { ChatUiMessage };

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
  // Whether the effective model can process image attachments. Drives the
  // chat's image-attach gate (button disabled, send blocked, images cleared
  // on switch to a non-capable model).
  modelVisionCapable: boolean;
  // Whether the effective model GENERATES images. Independent of
  // modelVisionCapable, which is whether it ACCEPTS them. Gates only what the
  // composer NEWLY OFFERS (the image-prompt affordances on an unpinned
  // thread, and the attach button, which an image run has no use for) — never
  // what an existing thread DOES: that is chatKind's job.
  modelImageCapable: boolean;
  // The THREAD's kind: "" (text) or "image". Pinned by the backend at the
  // thread's first send and authoritative from then on; while the thread is
  // still empty it follows the picked model, because that is what the next
  // send will pin it to. Render composer affordances off THIS, not off
  // modelImageCapable — a text thread stays a text thread even if the user
  // later picks an image model.
  chatKind: string;
  // The active chat's LIVE RUN's own reported kind ("" text | "image"),
  // read from useChatRuns' kindOf -- deliberately NOT chatKind above.
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
  // How many more generated images this chat is expected to hold before it
  // hits the backend's content cap, or null when that is unknown (a text
  // thread, or a gateway that serves no max_content_bytes). null means
  // "unknown" and must never be read as zero.
  imageCapacityLeft: number | null;
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
  handleFiles: (fileList: FileList | null) => Promise<void>;
  removeImage: (index: number) => void;
  setModel: Dispatch<SetStateAction<string>>;
  setSystemPrompt: Dispatch<SetStateAction<string>>;
  setTemperature: Dispatch<SetStateAction<number>>;
  setMaxTokens: Dispatch<SetStateAction<number>>;
  setSelectedTokenId: Dispatch<SetStateAction<string>>;
  handlersFor: (id: string) => { onEdit: (text: string) => void; onRegenerate: () => void };
};

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
  const [messages, setMessages] = useState<ChatUiMessage[]>([]);
  const [input, setInput] = useState('');
  const [images, setImages] = useState<string[]>([]);
  const [selectedTokenId, setSelectedTokenId] = useState<string>(DEFAULTS.run_as_token_id);
  const [serverOverride, setServerOverride] = useState<string>(DEFAULTS.server_override);
  const [serverOverrideForceUnreachable, setServerOverrideForceUnreachable] = useState<boolean>(
    DEFAULTS.server_override_force_unreachable,
  );
  // The distinct gateway models the selected override server offers (see
  // api.serverModels), used to narrow the chat's model dropdown once a server
  // override is picked. Empty while no override is set.
  const [serverOverrideModels, setServerOverrideModels] = useState<ServerModelOption[]>([]);

  // The caller's manageable servers (undefined prop -> []): drives whether the
  // server-override picker renders at all and its option list.
  const manageableServers = useMemo(() => servers ?? [], [servers]);

  // The THREAD's pinned kind as last loaded/established ("" text | "image").
  // Real state, not a value read once at load: the pin is established by the
  // thread's FIRST send, and a live session (new chat -> pick an image model
  // -> send -> send again -> regenerate, no reload) must see it immediately.
  // Opaque string, never narrowed to the kinds this build knows, so an
  // unfamiliar one survives the save round trip instead of being erased. ""
  // is text: the SETTING is optional (omitted for a text thread, mirroring
  // the backend's omitempty) but the state is always a string, so nothing
  // downstream has to handle undefined.
  const [pinnedChatKind, setPinnedChatKind] = useState<string>('');
  // The backend's per-chat content cap, as SERVED on the chat listing
  // (ChatListResponse.max_content_bytes = portal.MaxChatContentBytes). 0 = not
  // served (an older gateway) = capacity unknown. Never hardcoded here: a
  // second copy of the Go constant would drift from it silently, and the
  // failure mode is a capacity line stating the wrong number.
  const [maxContentBytes, setMaxContentBytes] = useState(0);

  // The chat list + which one is active.
  const [chats, setChats] = useState<ChatSummary[]>([]);
  const [activeChatId, setActiveChatId] = useState<string | null>(null);
  const [chatsLoading, setChatsLoading] = useState(true);

  const { showError } = useToast();

  const chatModels = useMemo(
    () => models.filter((option) => option.flavors.includes('openai')),
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

  // The kind the NEXT send would pin, derived from the picked model. Only
  // ever consulted while the thread has no transcript at all (below), which
  // is why the synthetic model option injected for a picked-but-uncatalogued
  // model — no capability fields, so image === false — is harmless here: the
  // PINNED kind is the thread's truth the moment one exists.
  const prospectiveKind = modelImageCapable ? 'image' : '';
  // The thread's kind, and the whole distinction in one line: once the thread
  // has a transcript the backend has already pinned it (PrepareChatRun forces
  // the stored kind on every send after the first), so the pin wins and the
  // currently-picked model is irrelevant; while it is still empty there is no
  // pin yet, so the composer shows what the next send would establish.
  const chatKind = messages.length > 0 ? pinnedChatKind : prospectiveKind;

  // Remaining room for generated images, computed only for an image thread:
  // measuring the transcript means serialising it, and a text thread neither
  // shows the number nor is gated on it. null = unknown (see the store type).
  const imageCapacityLeft = useMemo(
    () => (chatKind === 'image' ? imagesLeft(messages, maxContentBytes) : null),
    [chatKind, messages, maxContentBytes],
  );

  // Refs mirror the latest state so the stable callbacks below (send / edit /
  // regenerate / save) can read current values without being re-created every
  // render.
  const messagesRef = useRef(messages);
  const modelRef = useRef(model);
  const systemPromptRef = useRef(systemPrompt);
  const temperatureRef = useRef(temperature);
  const maxTokensRef = useRef(maxTokens);
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
  // Mirror the thread's kind so the stable send/edit/regenerate callbacks and
  // buildDoc (which read via refs, not props) always see the latest value.
  // Direct render-time assignment, like modelAvailableRef/modelVisionCapableRef
  // above — an effect would lag by one passive-effect flush, and this ref used
  // to be written only in activateChat, which made it permanently stale for
  // every thread pinned during the current session.
  const chatKindRef = useRef<string>(chatKind);
  chatKindRef.current = chatKind;
  // Likewise the remaining image capacity, for send()'s refusal.
  const imageCapacityLeftRef = useRef<number | null>(imageCapacityLeft);
  imageCapacityLeftRef.current = imageCapacityLeft;

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
      selectedTokenId,
      serverOverride,
      serverOverrideForceUnreachable,
      kind: chatKind,
    },
    isRunning,
    setChats,
  );
  const {
    buildDoc,
    flushSave,
    clearDirty,
    skipNextSave,
    cancelPendingSave,
    setTranscriptStale,
    isTranscriptStale,
    flushOnUnmount,
  } = persistence;

  // Wire persistence's dirty-clearing into the run engine's terminal callback
  // (replaces a direct dirtyRef write formerly inline in finishRun). Cheap ref
  // write on every render, matching this file's existing "read latest via a
  // stable ref, assigned at render time" style (see apiRef/showErrorRef above).
  onTerminal((chatId) => {
    if (chatId === activeChatIdRef.current) clearDirty();
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
    selectedTokenIdRef.current = selectedTokenId;
    serverOverrideRef.current = serverOverride;
    serverOverrideForceUnreachableRef.current = serverOverrideForceUnreachable;
    chatsRef.current = chats;
    activeChatIdRef.current = activeChatId;
  });

  // Cache per-message callbacks so memoized ChatMessage children keep stable
  // prop references across re-renders.
  const handlerCacheRef = useRef(
    new Map<string, { onEdit: (text: string) => void; onRegenerate: () => void }>(),
  );

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
        // The served content cap (portal.MaxChatContentBytes). Absent on a
        // gateway older than the field -> stays 0 -> capacity unknown, which
        // shows no number and refuses no send. Guessing one instead would be
        // the very drift this is served to avoid.
        if (typeof list?.max_content_bytes === 'number') setMaxContentBytes(list.max_content_bytes);
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
  // image still attached. Runs whenever either capability flips; a no-op
  // while there is nothing attached.
  //
  // The image-GENERATOR case is not the same refusal: such a model may well
  // be vision-capable too, so the vision check below would let the attachment
  // through — and the run would then drop it in silence, because an image
  // run's request body is {model, prompt, response_format} with no history
  // and no attachment at all. It is checked first, and says so in its own
  // words.
  useEffect(() => {
    if (images.length === 0) return;
    if (modelImageCapable) {
      setImages([]);
      showError(t.chatImageGeneratorNoInput);
      return;
    }
    if (!modelVisionCapable) {
      setImages([]);
      showError(t.chatImageModelUnsupported);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelVisionCapable, modelImageCapable]);

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

  // The run/SSE engine's subscribe/finishRun/adoptCanonicalTranscript/
  // ensureAssistant/writeAssistant/updateChatMessages/markRunning now live in
  // useChatRuns.ts (FA-2); `runs` above exposes the narrow interface this
  // provider needs (subscribeRun, isRunning, chatBuffers, onTerminal, ...).

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
      // An IMAGE thread is exempt because its request body carries no history
      // at all -- just {model, prompt, response_format} -- so there is no
      // vision input to refuse. Without this exemption an image thread refused
      // its own regenerate from the second turn onward, telling the user that a
      // model whose only purpose is images does not support images.
      if (
        chatKindRef.current !== 'image' &&
        !modelVisionCapableRef.current &&
        historyHasImage(history)
      ) {
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
    [subscribeRun, currentSettings, chatBuffers],
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
        const attachedImages = original.content.filter((part) => part.type === 'image_url');
        content = [{ type: 'text', text }, ...attachedImages];
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

  const handlersFor = useCallback(
    (id: string) => {
      const cache = handlerCacheRef.current;
      let handlers = cache.get(id);
      if (!handlers) {
        handlers = {
          onEdit: (text: string) => editUserMessage(id, text),
          onRegenerate: () => regenerate(id),
        };
        cache.set(id, handlers);
      }
      return handlers;
    },
    [editUserMessage, regenerate],
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
    const text = input.trim();
    if (!text && images.length === 0) return;
    if (images.length > 0 && !modelVisionCapableRef.current) {
      showErrorRef.current(tRef.current.chatImageModelUnsupported);
      return;
    }
    // Refuse an image send whose result this chat provably cannot store,
    // BEFORE anything is mutated or sent. An image generation costs minutes of
    // upstream CPU and produces a multi-megabyte artifact; the remaining
    // capacity is the one number in this feature that is exact and known
    // BEFORE the user commits. Spending the wait to then fail the save (the
    // run "completes" and the turn is dropped) is the failure this prevents.
    // A null capacity is UNKNOWN, not zero -- it must refuse nothing.
    const capacityLeft = imageCapacityLeftRef.current;
    if (chatKindRef.current === 'image' && capacityLeft !== null && capacityLeft <= 0) {
      showErrorRef.current(tRef.current.chatCapacityExhausted);
      return;
    }
    // This send pins the thread's kind server-side when the thread is still
    // empty (PrepareChatRun accepts the client's kind only on a genuine first
    // send). Record the same pin here, in the same batch as the optimistic
    // user bubble: from the next render on, `messages` is non-empty and the
    // thread's kind comes from the pin rather than from the picked model.
    if (messagesRef.current.length === 0) setPinnedChatKind(chatKindRef.current);

    let content: ChatContent = text;
    if (images.length > 0) {
      content = [
        { type: 'text', text },
        ...images.map((url) => ({ type: 'image_url' as const, image_url: { url } })),
      ];
    }
    maybeAutoTitle(text);
    const userMessage: ChatUiMessage = { id: nextId(), role: 'user', content };
    const next = [...messagesRef.current, userMessage];
    setMessages(next);
    messagesRef.current = next;
    chatBuffers.set(chatId, next);
    setInput('');
    setImages([]);

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

  const renameChat = useCallback(
    (id: string, title: string) => {
      const trimmed = title.trim();
      if (!trimmed) return;
      setChats((prev) => prev.map((chat) => (chat.id === id ? { ...chat, title: trimmed } : chat)));
      if (id === activeChatIdRef.current) activeTitleRef.current = trimmed;
      void (async () => {
        try {
          // The PUT contract requires title + content. Use the live document for
          // the active chat; otherwise fetch the target's content first.
          //
          // ...and NOT the live document when this client cannot vouch for it.
          // A rename is a fourth writer: it PUTs buildDoc() directly and never
          // touches flushSave, so the refusal the other three share does not
          // reach it. But refusing the rename outright would be its own
          // surprise -- the user asked to change a TITLE and would watch
          // nothing happen -- so it falls back to the branch this function
          // already has for every other chat and sends the SERVER's own stored
          // content back with the new title. The transcript is then written
          // unchanged (it is the server's own bytes) and the rename still
          // renames. If that fetch fails too, saveChat is never reached and
          // the catch below says so, which is loud rather than destructive.
          const stale = isTranscriptStale(id);
          const content =
            id === activeChatIdRef.current && !stale
              ? buildDoc()
              : normalizeDoc((await apiRef.current.chat(id)).content);
          const saved = await apiRef.current.saveChat(id, { title: trimmed, content });
          setChats((prev) =>
            byNewest(
              prev.map((chat) =>
                chat.id === id
                  ? { ...chat, title: saved.title, updated_at: saved.updated_at }
                  : chat,
              ),
            ),
          );
        } catch (err) {
          showErrorRef.current(formatPortalError(err, tRef.current));
        }
      })();
    },
    [buildDoc, isTranscriptStale],
  );

  const removeImage = useCallback(
    (index: number) => setImages((prev) => prev.filter((_, position) => position !== index)),
    [],
  );

  async function handleFiles(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return;
    // Local running counter: the closed-over `images.length` is stale across the
    // awaits below, so a multi-file select would never trip the cap mid-batch.
    let count = images.length;
    let lastError = '';
    for (const file of Array.from(fileList)) {
      if (count >= MAX_IMAGES) {
        lastError = t.chatImageErrorCount;
        break;
      }
      try {
        const url = await prepareImageDataUrl(file);
        setImages((prev) => (prev.length >= MAX_IMAGES ? prev : [...prev, url]));
        count += 1;
      } catch (err) {
        if (err instanceof ImageAttachError) {
          switch (err.reason) {
            case 'type':
              lastError = t.chatImageErrorType;
              break;
            case 'size':
              lastError = t.chatImageErrorSize;
              break;
            default:
              // "decode" (or any future reason) -> generic
              lastError = t.chatImageError;
          }
        } else {
          lastError = t.chatImageError;
        }
      }
    }
    // Set one trailing error so a multi-file batch shows a single toast rather
    // than flickering per file (and no stale error survives a later success).
    if (lastError) showError(lastError);
  }

  const value: ChatStore = {
    messages,
    streaming,
    runningChatIds,
    isChatRunning: (id: string) => runningChatIds.has(id),
    input,
    images,
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
    chatKind,
    runKind,
    runElapsedMs,
    imageCapacityLeft,
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
