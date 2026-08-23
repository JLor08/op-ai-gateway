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
  } = state;

  const saveTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const dirtyRef = useRef(false);
  // Set true when the caller loads a chat's content into state so the save
  // effect ignores the load-induced state changes (we must not immediately
  // re-save freshly-loaded content).
  const skipSaveRef = useRef(false);

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
      const payload = { title: activeTitleRef.current, content: buildDoc() };
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
    if (id && dirtyRef.current && !isRunning(id)) {
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
    flushOnUnmount,
  };
}
