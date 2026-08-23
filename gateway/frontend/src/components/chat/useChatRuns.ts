// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The run/SSE engine extracted out of ChatStore.tsx (FA-2): owns the per-chat
// run subscriptions + transcript buffers and exposes a narrow interface to the
// provider. Behavior (terminal-snapshot finalization, canonical-transcript
// adoption, buffer seeding, the "never apply a snapshot onto an unloaded chat"
// invariant) is unchanged from the original inline implementation.

import {
  useCallback,
  useRef,
  useState,
  type Dispatch,
  type RefObject,
  type SetStateAction,
} from 'react';
import type { Chat, ChatRunStatus, PortalApi } from '../../api';
import {
  messageStatusForRun,
  metricsOf,
  nextId,
  normalizeDoc,
  type ChatUiMessage,
  type RunMetricsPayload,
} from './chatDoc';

// A live run subscription for one chat. `es` is the open EventSource (null once
// terminal / after close). Keyed by chatId in runsRef.
type RunState = { runId: string; status: ChatRunStatus; es: EventSource | null };

// Shapes of the named SSE payloads the run endpoint emits (metrics keys are
// snake_case on the wire; mapped to the camelCase message fields below).
type SnapshotPayload = {
  reasoning?: string;
  content?: string;
  metrics?: RunMetricsPayload;
  status?: ChatRunStatus;
  error?: string;
};
type DeltaPayload = { reasoning?: string; content?: string };

// Invoked whenever a chat's run reaches a terminal state (completed / error /
// canceled / interrupted). The provider wires persistence's dirty-clearing
// through this — it REPLACES the direct dirtyRef write formerly inline in
// finishRun, so the run engine never reaches into persistence's bookkeeping.
export type RunTerminalHandler = (chatId: string) => void;

export type ChatRunsApi = {
  // Chats with a live server run (any chat, not just the active one). Mirrors
  // runsRef for RENDERING (sidebar spinner, derived `streaming`); may lag a
  // render behind the ref, so gating logic must use `isRunning` instead.
  runningChatIds: Set<string>;
  // Synchronous, always-current "is this chat's run live" check (reads the
  // backing ref directly, never stale). Use this for gating (send / edit /
  // save), never `runningChatIds`.
  isRunning: (chatId: string) => boolean;
  // The run status of chatId's most recent entry, or undefined when it never
  // had one (never run / not yet registered).
  statusOf: (chatId: string) => ChatRunStatus | undefined;
  // The live run's id for chatId, or undefined when it is not running.
  runIdIfRunning: (chatId: string) => string | undefined;
  // Per-chat transcript buffers: a background chat's streamed deltas land
  // here (not in the visible `messages`), and it is also the seed source for
  // (re)activating a chat. Exposed as the live Map itself (stable identity
  // across renders) so chat-load/CRUD code can read/seed it directly.
  buffers: Map<string, ChatUiMessage[]>;
  // Register a run's metadata WITHOUT opening its EventSource yet (bootstrap
  // replay registers every active run before the active chat's transcript is
  // loaded, so interrupted-detection never falsely fires for it).
  registerRunning: (chatId: string, runId: string) => void;
  // Open (or reopen) the SSE subscription for a chat's run.
  subscribe: (chatId: string, runId: string) => void;
  // Close a chat's EventSource and drop its run + buffer bookkeeping.
  forget: (chatId: string) => void;
  // Close every open EventSource without otherwise touching state (provider
  // unmount — the server run keeps going and is re-subscribed on next load).
  closeAll: () => void;
  // Register the terminal callback (the provider registers exactly one, at
  // render time, wiring persistence's dirty-clearing).
  onTerminal: (cb: RunTerminalHandler) => void;
};

export function useChatRuns(
  apiRef: RefObject<Pick<PortalApi, 'chat'>>,
  activeChatIdRef: RefObject<string | null>,
  messagesRef: RefObject<ChatUiMessage[]>,
  setMessages: Dispatch<SetStateAction<ChatUiMessage[]>>,
  onRefreshRef: RefObject<() => void | Promise<void>>,
  showErrorRef: RefObject<(message: string) => void>,
): ChatRunsApi {
  // Per-chat run subscriptions, keyed by chatId. The source of truth for "is
  // this chat running" (runningChatIds mirrors it for rendering). Entries are
  // kept after a run finishes (status becomes terminal, es null) so the chat
  // seeds its transcript from its buffer instead of reloading the server copy.
  const runsRef = useRef<Map<string, RunState>>(new Map());
  // Per-chat transcript buffers. A background chat's streamed deltas land here
  // (not in the visible `messages`), so switching to it shows the latest.
  const chatBuffersRef = useRef<Map<string, ChatUiMessage[]>>(new Map());
  const [runningChatIds, setRunningChatIds] = useState<Set<string>>(new Set());
  const onTerminalRef = useRef<RunTerminalHandler | null>(null);

  // Add/remove a chat from the running set (source of truth is runsRef; this
  // mirror drives rendering + the derived active-chat `streaming` flag).
  const markRunning = useCallback((chatId: string, running: boolean) => {
    setRunningChatIds((prev) => {
      if (running === prev.has(chatId)) return prev;
      const next = new Set(prev);
      if (running) next.add(chatId);
      else next.delete(chatId);
      return next;
    });
  }, []);

  // Apply an updater to a chat's transcript, keyed by chatId. The active chat's
  // transcript is the visible `messages`; a background chat's lives only in its
  // buffer. Both stay mirrored so switching to a chat shows its latest state.
  const updateChatMessages = useCallback(
    (chatId: string, updater: (prev: ChatUiMessage[]) => ChatUiMessage[]) => {
      const isActive = chatId === activeChatIdRef.current;
      const source = isActive ? messagesRef.current : (chatBuffersRef.current.get(chatId) ?? []);
      const nextList = updater(source);
      if (nextList === source) return;
      chatBuffersRef.current.set(chatId, nextList);
      if (isActive) {
        messagesRef.current = nextList;
        setMessages(nextList);
      }
    },
    [activeChatIdRef, messagesRef, setMessages],
  );

  // Ensure the chat's trailing message is the in-flight assistant bubble; append
  // an empty one when the last message is the user turn.
  const ensureAssistant = useCallback(
    (chatId: string) => {
      updateChatMessages(chatId, (prev) => {
        const last = prev.at(-1);
        if (last?.role === 'assistant') return prev;
        return [...prev, { id: nextId(), role: 'assistant', content: '', status: 'pending' }];
      });
    },
    [updateChatMessages],
  );

  // Apply an updater to the chat's trailing assistant message (no-op if the
  // trailing message is not an assistant).
  const writeAssistant = useCallback(
    (chatId: string, updater: (m: ChatUiMessage) => ChatUiMessage) => {
      updateChatMessages(chatId, (prev) => {
        if (prev.length === 0) return prev;
        const last = prev.at(-1)!;
        if (last.role !== 'assistant') return prev;
        const copy = prev.slice();
        copy[copy.length - 1] = updater(last);
        return copy;
      });
    },
    [updateChatMessages],
  );

  // Terminal handling for a chat's run: close its EventSource, record the final
  // status, stamp it on the assistant message (or prune a dangling empty bubble
  // left by an error/cancel before any output), and refresh tokens/models.
  const finishRun = useCallback(
    (chatId: string, status: ChatRunStatus, error?: string) => {
      const run = runsRef.current.get(chatId);
      if (run) {
        run.es?.close();
        runsRef.current.set(chatId, { runId: run.runId, status, es: null });
      }
      const msgStatus = messageStatusForRun(status);
      updateChatMessages(chatId, (prev) => {
        if (prev.length === 0) return prev;
        const last = prev.at(-1)!;
        if (last.role !== 'assistant') return prev;
        const empty =
          (typeof last.content === 'string' ? last.content.length === 0 : true) && !last.reasoning;
        if (empty) return prev.slice(0, -1);
        const copy = prev.slice();
        copy[copy.length - 1] = { ...last, status: msgStatus };
        return copy;
      });
      markRunning(chatId, false);
      // Replaces the formerly-inline `dirtyRef.current = false` write: the
      // provider wires persistence's dirty-clearing through this callback so
      // the canonical adopt (below) can re-dirty via the `messages` state
      // change without the stream-end flushSave racing ahead of it.
      onTerminalRef.current?.(chatId);
      if (error) showErrorRef.current(error);
      void onRefreshRef.current();
    },
    [updateChatMessages, markRunning, showErrorRef, onRefreshRef],
  );

  // Review follow-up #A: after a run's `done`, the backend has ALREADY committed
  // the canonical transcript (server message ids + `status`). Refetch it and
  // adopt it into this chat's buffer (and the visible `messages` when active) so
  // the buffer becomes authoritative — any later save is idempotent and a
  // reopen/interrupted check reads the server's transcript, never the
  // FE-generated one. Best-effort: a failed refetch leaves the buffer as-is; a
  // doc without a committed turn is left alone (never wipe streamed content the
  // server has not persisted). Guarded against a run that started meanwhile:
  // only adopt while THIS run is still the chat's (terminal) entry.
  const adoptCanonicalTranscript = useCallback(
    async (chatId: string, runId: string) => {
      let full: Chat;
      try {
        full = await apiRef.current.chat(chatId);
      } catch {
        return;
      }
      const entry = runsRef.current.get(chatId);
      if (entry?.runId !== runId || entry?.status === 'running') return;
      const canonical = normalizeDoc(full.content).messages;
      if (canonical.length === 0) return;
      chatBuffersRef.current.set(chatId, canonical);
      if (chatId === activeChatIdRef.current) {
        messagesRef.current = canonical;
        setMessages(canonical);
      }
    },
    [apiRef, activeChatIdRef, messagesRef, setMessages],
  );

  // Register a run's metadata WITHOUT opening its EventSource (bootstrap
  // replay, and the reopen path below before its transcript is seeded).
  const registerRunning = useCallback(
    (chatId: string, runId: string) => {
      runsRef.current.set(chatId, { runId, status: 'running', es: null });
      markRunning(chatId, true);
    },
    [markRunning],
  );

  // Subscribe to a server run's SSE stream for a chat. snapshot REPLACES the
  // in-flight assistant's reasoning/content/metrics (so an auto-reconnect replay
  // is safe); delta APPENDS; done applies the final snapshot then finishes.
  // Keyed by chatId — a background chat's events update its buffer, not the
  // visible transcript.
  //
  // INVARIANT: a run's snapshot/delta is only ever applied on top of the chat's
  // REAL (server-loaded) transcript, never an empty base. The live send/edit
  // path already has the chat active with its optimistic bubble in the buffer.
  // The reopen path (bootstrap) subscribes to a BACKGROUND chat whose transcript
  // has not been loaded yet, so we first seed that chat's buffer from its server
  // doc, then open the stream — otherwise the snapshot would fabricate a lone
  // assistant bubble on an empty base and, on switch, mask the real transcript.
  const subscribe = useCallback(
    (chatId: string, runId: string) => {
      runsRef.current.get(chatId)?.es?.close();

      // Open the EventSource and wire the run listeners. Assumes the chat's
      // buffer (or the active `messages`) already holds the real transcript.
      const openStream = () => {
        const es = new EventSource(
          `/api/portal/chats/${encodeURIComponent(chatId)}/runs/${encodeURIComponent(runId)}/events`,
        );
        runsRef.current.set(chatId, { runId, status: 'running', es });
        markRunning(chatId, true);

        es.addEventListener('snapshot', (e) => {
          const snap = JSON.parse((e as MessageEvent).data) as SnapshotPayload;
          ensureAssistant(chatId);
          writeAssistant(chatId, (m) => ({
            ...m,
            reasoning: snap.reasoning ?? '',
            content: snap.content ?? '',
            ...metricsOf(snap.metrics),
          }));
          // A late subscriber (reopen/reconnect just after a run finished) gets
          // the terminal state as a `snapshot` (status completed/error/canceled)
          // and then the stream closes with no `done` to follow. Finalize here
          // exactly like the `done` path, or the chat would stay marked running
          // forever (spinner + Stop, inputs disabled) until a full reload.
          if (snap.status && snap.status !== 'running') {
            finishRun(chatId, snap.status, snap.error);
            void adoptCanonicalTranscript(chatId, runId);
          }
        });
        es.addEventListener('delta', (e) => {
          const d = JSON.parse((e as MessageEvent).data) as DeltaPayload;
          ensureAssistant(chatId);
          writeAssistant(chatId, (m) => ({
            ...m,
            reasoning: (m.reasoning ?? '') + (d.reasoning ?? ''),
            content: (typeof m.content === 'string' ? m.content : '') + (d.content ?? ''),
          }));
        });
        es.addEventListener('done', (e) => {
          const term = JSON.parse((e as MessageEvent).data) as SnapshotPayload;
          ensureAssistant(chatId);
          writeAssistant(chatId, (m) => ({
            ...m,
            reasoning: term.reasoning ?? '',
            content: term.content ?? '',
            ...metricsOf(term.metrics),
          }));
          finishRun(chatId, term.status ?? 'completed', term.error);
          // Adopt the server-committed transcript (server ids + status) so the
          // buffer is canonical and never clobbered by the FE copy (follow-up #A).
          void adoptCanonicalTranscript(chatId, runId);
        });
        // Leave onerror to the browser: EventSource auto-reconnects and the
        // server replays a fresh snapshot, so no client recovery is needed.
        es.onerror = () => {};
      };

      // Fast path: the chat is active, or already has a streamed buffer (the
      // live send/edit/regenerate path). Its transcript is loaded — open now.
      const isActive = chatId === activeChatIdRef.current;
      const hasBuffer = (chatBuffersRef.current.get(chatId)?.length ?? 0) > 0;
      if (isActive || hasBuffer) {
        openStream();
        return;
      }

      // Reopen path: a BACKGROUND running chat whose transcript is not loaded.
      // Register its run metadata NOW (so switching to it treats it as running,
      // not interrupted), then seed its buffer from the server doc BEFORE
      // opening the stream. Best-effort: on a fetch failure just open the stream.
      registerRunning(chatId, runId);
      void (async () => {
        let docMessages: ChatUiMessage[] | null = null;
        try {
          const full = await apiRef.current.chat(chatId);
          docMessages = normalizeDoc(full.content).messages;
        } catch {
          /* best-effort: fall through and open without seeding */
        }
        // The chat may have been deleted, or a newer run registered, while we
        // awaited — only proceed if THIS run is still the chat's entry.
        const entry = runsRef.current.get(chatId);
        if (entry?.runId !== runId) return;
        if (docMessages && (chatBuffersRef.current.get(chatId)?.length ?? 0) === 0) {
          chatBuffersRef.current.set(chatId, docMessages);
          // If the chat was switched active meanwhile, reflect the doc into view.
          if (chatId === activeChatIdRef.current) {
            messagesRef.current = docMessages;
            setMessages(docMessages);
          }
        }
        openStream();
      })();
    },
    [
      activeChatIdRef,
      apiRef,
      messagesRef,
      setMessages,
      ensureAssistant,
      writeAssistant,
      finishRun,
      adoptCanonicalTranscript,
      registerRunning,
      markRunning,
    ],
  );

  const isRunning = useCallback(
    (chatId: string) => runsRef.current.get(chatId)?.status === 'running',
    [],
  );
  const statusOf = useCallback(
    (chatId: string): ChatRunStatus | undefined => runsRef.current.get(chatId)?.status,
    [],
  );
  const runIdIfRunning = useCallback((chatId: string): string | undefined => {
    const run = runsRef.current.get(chatId);
    return run?.status === 'running' ? run.runId : undefined;
  }, []);

  // Close a chat's EventSource and drop its run/buffer bookkeeping (deleteChat;
  // the backend DELETE also cancels the server-side run).
  const forget = useCallback(
    (chatId: string) => {
      runsRef.current.get(chatId)?.es?.close();
      runsRef.current.delete(chatId);
      chatBuffersRef.current.delete(chatId);
      markRunning(chatId, false);
    },
    [markRunning],
  );

  const closeAll = useCallback(() => {
    for (const run of runsRef.current.values()) run.es?.close();
  }, []);

  const onTerminal = useCallback((cb: RunTerminalHandler) => {
    onTerminalRef.current = cb;
  }, []);

  return {
    runningChatIds,
    isRunning,
    statusOf,
    runIdIfRunning,
    buffers: chatBuffersRef.current,
    registerRunning,
    subscribe,
    forget,
    closeAll,
    onTerminal,
  };
}
