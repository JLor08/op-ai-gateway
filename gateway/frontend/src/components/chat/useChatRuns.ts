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
import { formatChatRunErrorCode } from '../shared/format';
import type { Translation } from '../shared/types';
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
type RunState = {
  runId: string;
  status: ChatRunStatus;
  es: EventSource | null;
  // The run's server-measured age: the ms value from its own last
  // snapshot/done/201/active-runs-listing payload ("serverMs"), paired with
  // the performance.now() reading taken the instant THIS client read it
  // ("anchor"). elapsedMsOf below interpolates from this single pair between
  // snapshots -- anchoring on the SERVER's own measurement, never on the
  // local moment of Send, is what lets a reopened tab and the sending tab
  // agree on the same true elapsed time (see startRunResponse's doc comment,
  // chat_run_endpoints.go). Re-anchored (a fresh serverMs + a fresh anchor)
  // whenever a newer snapshot/done arrives; frozen from the terminal one on.
  //
  // Deliberately carries no `kind` field: the thread's kind is already real
  // state on the ChatStore (chatKind/pinnedChatKind, landed by the previous
  // task), and the composer's pending render reads THAT rather than a second
  // copy re-derived here from the run's own wire `kind` -- one source for
  // "what kind of thread is this", not two that could drift apart.
  age: { serverMs: number; anchor: number };
};

// Shapes of the named SSE payloads the run endpoint emits (metrics keys are
// snake_case on the wire; mapped to the camelCase message fields below).
// `kind`/`elapsed_ms` ride on snapshot/done only (never delta -- see the
// backend's own runEvent doc comment, chat_runs.go); `kind` is read here for
// wire-shape completeness but not consumed (see RunState's doc comment above
// for why), only `elapsed_ms` feeds the age anchor.
type SnapshotPayload = {
  reasoning?: string;
  content?: string;
  metrics?: RunMetricsPayload;
  status?: ChatRunStatus;
  error?: string;
  kind?: string;
  elapsed_ms?: number;
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
  // chatId's live run's CURRENT elapsed ms, interpolated from its last known
  // server anchor to "now" (performance.now()) at call time. Undefined when
  // chatId has no live run. See RunState's `age` field for why this anchors
  // on the server's own measurement rather than the local moment of Send.
  elapsedMsOf: (chatId: string) => number | undefined;
  // Per-chat transcript buffers: a background chat's streamed deltas land
  // here (not in the visible `messages`), and it is also the seed source for
  // (re)activating a chat. Exposed as the live Map itself (stable identity
  // across renders) so chat-load/CRUD code can read/seed it directly.
  buffers: Map<string, ChatUiMessage[]>;
  // Register a run's metadata WITHOUT opening its EventSource yet (bootstrap
  // replay registers every active run before the active chat's transcript is
  // loaded, so interrupted-detection never falsely fires for it). `elapsedMs`
  // seeds the age anchor (from activeRunDTO for a bootstrap replay); omit it
  // for a run that has genuinely just started, where 0 is the true age.
  registerRunning: (chatId: string, runId: string, elapsedMs?: number) => void;
  // Open (or reopen) the SSE subscription for a chat's run. `elapsedMs` seeds
  // the age anchor exactly like registerRunning's, until the stream's own
  // first snapshot supplies a fresher one.
  subscribe: (chatId: string, runId: string, elapsedMs?: number) => void;
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
  // Used ONLY to localize a terminal run's error CODE (formatChatRunErrorCode)
  // before it reaches the toast -- every other string in this file is
  // either already a translated caller-supplied value or a raw wire code
  // this module has no business rendering untranslated.
  tRef: RefObject<Translation>,
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
        // Spread (not a fresh literal): `age` carries over as-is, so the
        // clock FREEZES at whatever it last was (the terminal snapshot/done's
        // own elapsed_ms, applied below in the snapshot/done listeners before
        // this is called) rather than being re-derived from a fresh
        // performance.now() that would make it start ticking again.
        runsRef.current.set(chatId, { ...run, status, es: null });
      }
      const msgStatus = messageStatusForRun(status);
      updateChatMessages(chatId, (prev) => {
        if (prev.length === 0) return prev;
        const last = prev.at(-1)!;
        if (last.role !== 'assistant') return prev;
        // "Empty" means the run wrote nothing into this bubble. Length is the
        // right test for BOTH shapes -- a string's characters and a content
        // array's parts -- and `typeof content === 'string' ? … : true` was
        // not: it called every structured content empty, so an image turn's
        // bubble was deleted the moment it arrived. pruneEmptyAssistantTail
        // (chatDoc.ts) already tests length, and this now agrees with it.
        const empty = last.content.length === 0 && !last.reasoning;
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
      // error is a WIRE CODE (runTimedOutMessage, ErrChatTooLarge.Error(),
      // ...), not prose -- localize it through errorLabelByCode before it
      // reaches the toast, or every chat-run code this task just mapped
      // would still show up as raw English (see format.ts's own doc comment
      // on formatChatRunErrorCode).
      if (error) showErrorRef.current(formatChatRunErrorCode(error, tRef.current));
      void onRefreshRef.current();
    },
    [updateChatMessages, markRunning, showErrorRef, onRefreshRef, tRef],
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
    (chatId: string, runId: string, elapsedMs = 0) => {
      runsRef.current.set(chatId, {
        runId,
        status: 'running',
        es: null,
        age: { serverMs: elapsedMs, anchor: performance.now() },
      });
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
    (chatId: string, runId: string, elapsedMs = 0) => {
      runsRef.current.get(chatId)?.es?.close();

      // Open the EventSource and wire the run listeners. Assumes the chat's
      // buffer (or the active `messages`) already holds the real transcript.
      const openStream = () => {
        const es = new EventSource(
          `/api/portal/chats/${encodeURIComponent(chatId)}/runs/${encodeURIComponent(runId)}/events`,
        );
        // Seed the age anchor from THIS call's own elapsedMs (the 201's, ~0
        // for a fresh send/regenerate, or activeRunDTO's real value for a
        // bootstrap replay) -- never from any entry already in runsRef, which
        // could belong to an unrelated PRIOR run this same chat once had.
        runsRef.current.set(chatId, {
          runId,
          status: 'running',
          es,
          age: { serverMs: elapsedMs, anchor: performance.now() },
        });
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
          // Re-anchor from this snapshot's OWN elapsed_ms before any terminal
          // handling below (finishRun copies the run's CURRENT age verbatim,
          // so it must already be the frozen, final one by the time it runs).
          const run = runsRef.current.get(chatId);
          if (run && typeof snap.elapsed_ms === 'number') {
            runsRef.current.set(chatId, {
              ...run,
              age: { serverMs: snap.elapsed_ms, anchor: performance.now() },
            });
          }
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
          // Re-anchor from the terminal event's own elapsed_ms BEFORE
          // finishRun: from here on the age must read as the run's real,
          // frozen duration -- not the running age from the last snapshot.
          const run = runsRef.current.get(chatId);
          if (run && typeof term.elapsed_ms === 'number') {
            runsRef.current.set(chatId, {
              ...run,
              age: { serverMs: term.elapsed_ms, anchor: performance.now() },
            });
          }
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
      registerRunning(chatId, runId, elapsedMs);
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
  // chatId's live run's current elapsed ms: its last known server anchor,
  // interpolated to "now" by the performance.now() gap since it was taken.
  // Called at render time (not itself reactive), so a caller that wants this
  // to keep ticking on screen re-derives it every render, same as `now =
  // Date.now()` in ActiveRequestsPanel -- the difference here is only WHICH
  // clock anchors the value (see RunState's doc comment on `age`).
  //
  // Once the run is TERMINAL, `age.serverMs` already IS its final duration
  // (the snapshot/done listener re-anchored it from the terminal event's own
  // elapsed_ms), so it is returned verbatim, with NO further
  // performance.now() gap added. Without this guard a chat left open past a
  // run's completion would silently grow this value on every unrelated
  // re-render (typing, switching models, ...) even though nothing is
  // rendering it right now -- exactly the re-derived-after-terminal drift
  // constraint 3 forbids, just one call deeper than where it would be
  // visible. A late subscriber inside the run's eviction grace must be told
  // how long the run TOOK, not how long ago the registry last touched it.
  const elapsedMsOf = useCallback((chatId: string): number | undefined => {
    const run = runsRef.current.get(chatId);
    if (!run) return undefined;
    if (run.status !== 'running') return run.age.serverMs;
    return run.age.serverMs + (performance.now() - run.age.anchor);
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
    elapsedMsOf,
    buffers: chatBuffersRef.current,
    registerRunning,
    subscribe,
    forget,
    closeAll,
    onTerminal,
  };
}
