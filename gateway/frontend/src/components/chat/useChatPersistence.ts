// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The active-chat persistence layer extracted out of ChatStore.tsx (FA-2):
// owns buildDoc/flushSave, the debounced-save effect, the pagehide keepalive,
// and the unmount flush, behind a narrow interface. Takes an injected
// `isRunning` predicate instead of reading the run engine's bookkeeping
// directly, so persistence has no dependency on useChatRuns.ts. Behavior
// (debounce timing, the 60KB keepalive cap, the "skip save while a run is
// live" guard) is unchanged from the original inline implementation.

import {
  useCallback,
  useEffect,
  useRef,
  type Dispatch,
  type RefObject,
  type SetStateAction,
} from 'react';
import type { ChatSummary, PortalApi } from '../../api';
import type { Translation } from '../shared/types';
import { formatPortalError } from '../shared/format';
import {
  byNewest,
  kindSetting,
  pruneEmptyAssistantTail,
  SAVE_DEBOUNCE_MS,
  type ActiveChatDoc,
  type ChatUiMessage,
} from './chatDoc';

export type ChatPersistenceApi = {
  // Build the opaque content document from the current active-chat state.
  // Exposed (not just used internally) because renameChat also needs it for
  // the active chat's PUT.
  buildDoc: () => ActiveChatDoc;
  // Persist the active chat now (cancels any pending debounce). Best-effort:
  // errors surface a toast and leave the chat marked dirty for a later retry.
  flushSave: () => Promise<void>;
  // Clear the dirty flag without touching the pending-save timer. Wired to
  // the run engine's onTerminal callback (replaces a direct dirtyRef write
  // formerly inline in finishRun): the backend committed the canonical
  // transcript before emitting `done`, so the stream-end flushSave must not
  // PUT the FE buffer over it; the canonical adopt re-dirties via `messages`.
  clearDirty: () => void;
  // Mark the NEXT content/settings-change effect run a no-op (used right
  // after loading a chat's content into state, so the load itself doesn't
  // trigger an immediate re-save; also used by newChat's failure fallback).
  skipNextSave: () => void;
  // Cancel any pending debounced save and clear dirty, WITHOUT skipping the
  // next change (used when discarding the active chat's pending save outside
  // a load — e.g. deleteChat).
  cancelPendingSave: () => void;
  // Mark (or clear) a chat whose LOCAL transcript is not known to match the
  // server's committed one, which blocks every save path below for that chat.
  //
  // It exists because a PUT full-replaces the stored content blob with no
  // merge: saving a transcript this client cannot vouch for does not risk a
  // conflict, it destroys whatever the server holds that the client is
  // missing. The one producer is the run engine's post-terminal canonical
  // refetch (useChatRuns' adoptCanonicalTranscript), which marks BEFORE its
  // await and clears only once it has adopted the server's own answer — a
  // refetch of a multi-megabyte image document can easily outlast the 800 ms
  // debounce, so cancelling a pending save in the failure handler would be
  // too late by then. Cleared again by a successful adopt, by deleteChat, and
  // by activating a chat straight from a freshly loaded server document.
  setTranscriptStale: (chatId: string, stale: boolean) => void;
  // Whether chatId carries that mark. Read by writers that live OUTSIDE this
  // module and therefore cannot be refused from inside it — renameChat PUTs
  // buildDoc() for the active chat directly, bypassing flushSave, so it has
  // to ask. The invariant is "no write may carry an unproven local
  // transcript", not "these N functions check a flag": a writer that cannot
  // refuse must re-derive its document from the server instead.
  isTranscriptStale: (chatId: string) => boolean;
  // Final best-effort flush on a real provider unmount (logout): cancels the
  // pending timer and, unless a run is live, fires a synchronous-dispatch
  // save for the active chat if it is dirty.
  flushOnUnmount: () => void;
};

export function useChatPersistence(
  refs: {
    activeChatIdRef: RefObject<string | null>;
    messagesRef: RefObject<ChatUiMessage[]>;
    modelRef: RefObject<string>;
    systemPromptRef: RefObject<string>;
    temperatureRef: RefObject<number>;
    maxTokensRef: RefObject<number>;
    selectedTokenIdRef: RefObject<string>;
    serverOverrideRef: RefObject<string>;
    serverOverrideForceUnreachableRef: RefObject<boolean>;
    // The active thread's pinned kind ("" text | "image"). Read here because
    // buildDoc has to write it back: the backend's PUT full-replaces the
    // opaque content blob with NO merge, so any setting missing from buildDoc
    // is erased from the stored document on the very next autosave — which is
    // exactly how the server-side kind pin was being destroyed before it was
    // threaded through here.
    chatKindRef: RefObject<string>;
    activeTitleRef: RefObject<string>;
    apiRef: RefObject<Pick<PortalApi, 'saveChat' | 'saveChatKeepalive'>>;
    showErrorRef: RefObject<(message: string) => void>;
    tRef: RefObject<Translation>;
  },
  // Reactive state: any change schedules a debounced save of the active chat.
  state: {
    activeChatId: string | null;
    messages: ChatUiMessage[];
    model: string;
    systemPrompt: string;
    temperature: number;
    maxTokens: number;
    selectedTokenId: string;
    serverOverride: string;
    serverOverrideForceUnreachable: boolean;
    // A NEW PERSISTED SETTING NEEDS THREE LOCKSTEP EDITS IN THIS FILE: this
    // type, the destructure below, and the debounced effect's EXPLICIT dep
    // array (it sits under an eslint-disable for exhaustive-deps, so a missed
    // dep is silent — the setting simply never schedules a save).
    kind: string;
  },
  // Synchronous, always-current "is this chat's run live" check (see
  // useChatRuns.ts); injected so persistence never reaches into the run
  // engine's own bookkeeping.
  isRunning: (chatId: string) => boolean,
  setChats: Dispatch<SetStateAction<ChatSummary[]>>,
): ChatPersistenceApi {
  const {
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
  } = refs;
  const {
    activeChatId,
    messages,
    model,
    systemPrompt,
    temperature,
    maxTokens,
    selectedTokenId,
    serverOverride,
    serverOverrideForceUnreachable,
    kind,
  } = state;

  const saveTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const dirtyRef = useRef(false);
  // Set true when the caller loads a chat's content into state so the save
  // effect ignores the load-induced state changes (we must not immediately
  // re-save freshly-loaded content).
  const skipSaveRef = useRef(false);
  // Chats whose local transcript is not known to match the server's committed
  // one; every save path below refuses them. Per-CHAT rather than a single
  // flag because the run engine adopts background chats too, and
  // activateChat can seed a chat from its buffer rather than from the server
  // doc — so a background chat's unproven buffer must still be refused once
  // it becomes the active one. See setTranscriptStale in the API type above.
  const staleChatsRef = useRef<Set<string>>(new Set());

  // Build the opaque content document from the current active-chat state.
  const buildDoc = useCallback(
    (): ActiveChatDoc => ({
      settings: {
        model: modelRef.current,
        system_prompt: systemPromptRef.current,
        temperature: temperatureRef.current,
        max_tokens: maxTokensRef.current,
        run_as_token_id: selectedTokenIdRef.current,
        server_override: serverOverrideRef.current,
        server_override_force_unreachable: serverOverrideForceUnreachableRef.current,
        // Omitted entirely for a text thread, mirroring the backend's
        // `omitempty` -- see kindSetting in chatDoc.ts.
        ...kindSetting(chatKindRef.current),
      },
      messages: pruneEmptyAssistantTail(messagesRef.current),
    }),
    [
      modelRef,
      systemPromptRef,
      temperatureRef,
      maxTokensRef,
      selectedTokenIdRef,
      serverOverrideRef,
      serverOverrideForceUnreachableRef,
      chatKindRef,
      messagesRef,
    ],
  );

  // Persist the active chat now (cancels any pending debounce). Best-effort:
  // errors surface a toast and leave the chat marked dirty for a later retry.
  const flushSave = useCallback(async () => {
    if (saveTimerRef.current) {
      clearTimeout(saveTimerRef.current);
      saveTimerRef.current = null;
    }
    const id = activeChatIdRef.current;
    if (!id || !dirtyRef.current) return;
    // The server owns the transcript tail while a run is live — never PUT over
    // it (would clobber the just-committed / in-flight assistant turn).
    if (isRunning(id)) return;
    // ...and never PUT a transcript this client cannot vouch for. dirty is
    // deliberately LEFT SET: the content still needs saving, just not from
    // this copy, so the next save after the chat is reloaded picks it up.
    if (staleChatsRef.current.has(id)) return;
    dirtyRef.current = false;
    try {
      const saved = await apiRef.current.saveChat(id, {
        title: activeTitleRef.current,
        content: buildDoc(),
      });
      setChats((prev) =>
        byNewest(
          prev.map((chat) =>
            chat.id === id ? { ...chat, title: saved.title, updated_at: saved.updated_at } : chat,
          ),
        ),
      );
    } catch (err) {
      dirtyRef.current = true;
      showErrorRef.current(formatPortalError(err, tRef.current));
    }
  }, [buildDoc, isRunning, activeChatIdRef, activeTitleRef, apiRef, setChats, showErrorRef, tRef]);

  // Mark the active chat dirty on any content/settings change and schedule a
  // debounced save. Skipped while the active chat has a live run (the server
  // owns its transcript — a per-delta save would clobber it) and right after a
  // load (skipSaveRef). The final transcript is flushed by the caller's
  // stream-end effect once the run reaches a terminal state.
  useEffect(() => {
    if (!activeChatId) return;
    if (skipSaveRef.current) {
      skipSaveRef.current = false;
      return;
    }
    dirtyRef.current = true;
    if (isRunning(activeChatId)) return;
    // No staleChatsRef check here on purpose: the timer's only action is
    // flushSave, which refuses a stale chat itself, and a second copy of the
    // rule here would be one no test could distinguish from its absence.
    if (saveTimerRef.current) clearTimeout(saveTimerRef.current);
    saveTimerRef.current = setTimeout(() => {
      void flushSave();
    }, SAVE_DEBOUNCE_MS);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    messages,
    model,
    systemPrompt,
    temperature,
    maxTokens,
    selectedTokenId,
    serverOverride,
    serverOverrideForceUnreachable,
    kind,
    activeChatId,
    flushSave,
  ]);

  // Best-effort flush on page hide (reload, tab close, external navigation): the
  // provider-unmount flush does NOT reliably run on a full page unload, so a
  // debounced edit made in the last ~800ms could otherwise be lost. A keepalive
  // PUT survives unload. keepalive caps the body near 64 KB; larger transcripts
  // fall back to the debounce/stream-end save above (they change rarely enough
  // that the in-flight window is negligible).
  useEffect(() => {
    const flushOnHide = () => {
      const id = activeChatIdRef.current;
      if (!id || !dirtyRef.current) return;
      // Server owns the transcript while a run is live — skip the keepalive PUT.
      if (isRunning(id)) return;
      // Same refusal as flushSave: an unproven buffer must not be written,
      // and least of all by the one path whose failure is invisible.
      if (staleChatsRef.current.has(id)) return;
      const payload = { title: activeTitleRef.current, content: buildDoc() };
      // Skipped, and deliberately silently. For EVERY image thread this is the
      // taken branch (a single inline base64 image is far past 60 KB), so the
      // keepalive does not exist for them at all.
      //
      // The cap stays: it is the real keepalive body ceiling (~64 KB), and
      // raising it moves the failure from this `return` to the browser
      // dropping the PUT, which is strictly worse because it is then invisible
      // on BOTH sides. dirtyRef is deliberately NOT cleared, so the chat stays
      // dirty and the unmount flush (logout) and the next change both retry.
      //
      // WHAT IS EXPOSED: every persisted change made since the last debounced
      // save COMPLETED. The debounced save above has no size limit and is the
      // path that actually persists an image turn, but it is a TRAILING
      // debounce that RESETS its timer on each change -- so SAVE_DEBOUNCE_MS
      // (800 ms) is a settle window, not a ceiling. After an 800 ms pause
      // everything is on the server and this skip costs nothing; under a
      // stream of changes arriving faster than that (dragging the temperature
      // slider, typing a system prompt without pausing) the save never fires
      // and ALL of those changes are exposed, for as long as it continues.
      // Bounding that would mean a max-wait debounce, which is a change to
      // every chat and not just to image threads.
      //
      // Not exposed: the composer's own text (`input` is not a persisted
      // setting, so ordinary prompt typing never dirties the chat), and
      // anything during a live run (both this handler and the debounced effect
      // bail on isRunning -- the server owns that transcript and the
      // stream-end flush covers it).
      //
      // No console.warn and no toast: a warning reaches nobody at the moment of
      // a navigation, and pagehide also fires when the page enters the bfcache,
      // where a toast would alarm the user on their way back for what is
      // usually not a loss at all.
      if (JSON.stringify(payload).length > 60000) return;
      apiRef.current.saveChatKeepalive(id, payload);
      dirtyRef.current = false;
    };
    window.addEventListener('pagehide', flushOnHide);
    return () => window.removeEventListener('pagehide', flushOnHide);
  }, [buildDoc, isRunning, activeChatIdRef, activeTitleRef, apiRef]);

  const clearDirty = useCallback(() => {
    dirtyRef.current = false;
  }, []);

  const skipNextSave = useCallback(() => {
    skipSaveRef.current = true;
  }, []);

  const cancelPendingSave = useCallback(() => {
    if (saveTimerRef.current) {
      clearTimeout(saveTimerRef.current);
      saveTimerRef.current = null;
    }
    dirtyRef.current = false;
  }, []);

  const setTranscriptStale = useCallback((chatId: string, stale: boolean) => {
    if (stale) staleChatsRef.current.add(chatId);
    else staleChatsRef.current.delete(chatId);
  }, []);

  const isTranscriptStale = useCallback((chatId: string) => staleChatsRef.current.has(chatId), []);

  // Final best-effort flush on a real provider unmount (logout — a
  // client-side state change, NOT a page reload, so this fires reliably
  // unlike the pagehide path). The caller (ChatStoreProvider) invokes this
  // from its own unmount effect alongside closing the run engine's
  // EventSources.
  const flushOnUnmount = useCallback(() => {
    if (saveTimerRef.current) {
      clearTimeout(saveTimerRef.current);
      saveTimerRef.current = null;
    }
    const id = activeChatIdRef.current;
    if (id && dirtyRef.current && !isRunning(id) && !staleChatsRef.current.has(id)) {
      dirtyRef.current = false;
      void apiRef.current
        .saveChat(id, { title: activeTitleRef.current, content: buildDoc() })
        .catch(() => {});
    }
  }, [buildDoc, isRunning, activeChatIdRef, activeTitleRef, apiRef]);

  return {
    buildDoc,
    flushSave,
    clearDirty,
    skipNextSave,
    cancelPendingSave,
    setTranscriptStale,
    isTranscriptStale,
    flushOnUnmount,
  };
}
