// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useRef, useState } from 'react';
import type { PortalModelMapping, RuntimeSpec, RuntimeStatus } from '../../api';
import type { PortalApi } from './types';
import { isClusterHead } from './clusterTotals';

/**
 * How often the runtime screen re-reads the runtime spec of every cluster head
 * while a tab that shows heads is open. The cluster status, the withheld
 * reason, the totals and the stored peaks live on the head's spec, not on the
 * status stream, so without a re-read they would only show what was true when
 * the page loaded. A change of a head's live state triggers a re-read at once;
 * this interval catches what moves without one (a member's cooldown, a new
 * breach or peak).
 */
export const CLUSTER_SPEC_REFRESH_MS = 10_000;

/**
 * How often a head is re-read while its cluster forms, or while a start the
 * operator asked for has not shown up yet. The members run on other servers,
 * whose status streams this screen does not follow: the head's
 * cluster_status, read from the gateway's orchestration, is the only place
 * their phases show.
 */
export const CLUSTER_START_REFRESH_MS = 1_500;

const SEP = '\u0000';

export interface ClusterHeadRefresh {
  /** The operator pressed Start on this head's mapping. */
  requestStart: (mappingId: string) => void;
  /** The start answered: accepted, or refused (its error is shown elsewhere). */
  settleStart: (mappingId: string, accepted: boolean) => void;
  /** Whether a start was asked for and the head does not show it yet. */
  startPending: (mappingId: string) => boolean;
}

/**
 * How long the notice of an accepted start waits for the start to show in
 * the head's status before it gives up. The gateway begins the start before
 * it answers, so the first read after the answer normally shows it; this
 * bounds the fast cadence should it not.
 */
export const CLUSTER_START_NOTICE_MS = 10_000;

// What a head's cluster status said when Start was pressed: a start shows
// once it says something else, or once the cluster forms. A formed cluster
// stopped and started again still says `formed` until the new start begins,
// so `formed` alone is no sign of it.
function statusMark(spec: RuntimeSpec | undefined): string {
  const s = spec?.cluster_status;
  if (!s) return '';
  return [s.state, s.phase ?? '', s.cooldown_until ?? '', s.last_failure?.at ?? ''].join('|');
}

function startVisible(spec: RuntimeSpec | undefined, markAtPress: string): boolean {
  return forming(spec) || statusMark(spec) !== markAtPress;
}

function forming(spec: RuntimeSpec | undefined): boolean {
  return spec?.cluster_status?.state === 'forming';
}

// A digest of the live rows that can move a cluster: the heads' own rows
// (state and error) and every member row on this server.
function liveSignature(statusRows: RuntimeStatus[], headSpecIds: ReadonlySet<string>): string {
  return statusRows
    .filter((r) => r.kind === 'rpc_server' || headSpecIds.has(r.spec_id))
    .map((r) => `${r.spec_id}#${r.instance}:${r.state}:${r.last_error?.code ?? ''}`)
    .sort((a, b) => a.localeCompare(b))
    .join('|');
}

/**
 * Keeps the cluster heads' specs current while a tab that shows them is
 * open: every CLUSTER_SPEC_REFRESH_MS, at once when the status stream
 * reports a change that can move a cluster or the shown tab changes, and
 * every CLUSTER_START_REFRESH_MS for a head whose cluster forms or whose
 * start was just asked for. Each head has at most one read in flight. The
 * reads go through the caller's ordered-read pair, so a read that overlaps a
 * write is refused; a failed read raises no toast and is simply repeated.
 */
export function useClusterHeadRefresh({
  api,
  mappings,
  specsById,
  statusRows,
  view,
  active,
  beginRead,
  commitRead,
  mountedRef,
}: {
  api: Pick<PortalApi, 'runtimeSpec'>;
  mappings: PortalModelMapping[];
  specsById: Record<string, RuntimeSpec>;
  statusRows: RuntimeStatus[];
  /** The tab shown; showing another one that has heads re-reads them. */
  view: string;
  /** Whether the shown tab shows cluster heads. */
  active: boolean;
  beginRead: (mappingId: string) => number;
  commitRead: (mappingId: string, seen: number, spec: RuntimeSpec) => boolean;
  mountedRef: { current: boolean };
}): ClusterHeadRefresh {
  // The heads whose Start was pressed, with their status mark at the press.
  const [requested, setRequested] = useState<ReadonlyMap<string, string>>(new Map());
  const noticeTimersRef = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const inFlightRef = useRef(new Map<string, Promise<void>>());
  // The read pair changes identity on every render of the caller; the reads
  // always use the latest.
  const readRef = useRef({ beginRead, commitRead });
  readRef.current = { beginRead, commitRead };

  const headIds = mappings.filter((m) => isClusterHead(specsById[m.id])).map((m) => m.id);
  const headKey = headIds.join(SEP);
  const headSpecIds = new Set(
    headIds.map((id) => specsById[id]?.id).filter((id): id is string => Boolean(id)),
  );
  // Without a head there is nothing to follow, and no frame needs reading.
  const signature = headSpecIds.size > 0 ? liveSignature(statusRows, headSpecIds) : '';
  const fastKey = headIds.filter((id) => forming(specsById[id]) || requested.has(id)).join(SEP);
  const refreshing = active && headKey !== '';

  // One read per head in flight. Asked again meanwhile, the head gets one
  // more read once that one ends (however often it was asked), so a read
  // asked for after a change never answers with what was read before it.
  const queuedRef = useRef(new Map<string, Promise<void>>());
  const startRead = useCallback(
    (id: string): Promise<void> => {
      queuedRef.current.delete(id);
      const seen = readRef.current.beginRead(id);
      const read = api
        .runtimeSpec(id)
        .then(
          (spec) => {
            if (mountedRef.current) readRef.current.commitRead(id, seen, spec);
          },
          () => {},
        )
        .finally(() => inFlightRef.current.delete(id));
      inFlightRef.current.set(id, read);
      return read;
    },
    [api, mountedRef],
  );
  const refreshOne = useCallback(
    (id: string): Promise<void> => {
      const running = inFlightRef.current.get(id);
      if (!running) return startRead(id);
      let queued = queuedRef.current.get(id);
      if (!queued) {
        queued = running.then(() => startRead(id));
        queuedRef.current.set(id, queued);
      }
      return queued;
    },
    [startRead],
  );

  useEffect(() => {
    if (!refreshing) return undefined;
    const ids = headKey.split(SEP);
    const timer = setInterval(() => ids.forEach(refreshOne), CLUSTER_SPEC_REFRESH_MS);
    return () => clearInterval(timer);
  }, [refreshing, headKey, refreshOne]);

  useEffect(() => {
    if (!refreshing || fastKey === '') return undefined;
    const ids = fastKey.split(SEP);
    const timer = setInterval(() => ids.forEach(refreshOne), CLUSTER_START_REFRESH_MS);
    return () => clearInterval(timer);
  }, [refreshing, fastKey, refreshOne]);

  // What the last run saw for the same set of heads. A changed set of heads
  // (the lazy load finishing, a head added or removed) only records the new
  // signature: those heads were just read. Showing a tab with heads again, or
  // another one, re-reads them like a changed signature does.
  const lastRef = useRef<{ heads: string; signature: string; view: string } | null>(null);
  useEffect(() => {
    const prev = lastRef.current;
    const shown = refreshing ? view : '';
    lastRef.current = { heads: headKey, signature, view: shown };
    if (!refreshing || prev?.heads !== headKey) return;
    if (prev.view === shown && prev.signature === signature) return;
    headKey.split(SEP).forEach(refreshOne);
  }, [refreshing, headKey, signature, view, refreshOne]);

  const drop = useCallback(
    (ids: readonly string[]) => {
      if (!mountedRef.current || ids.length === 0) return;
      for (const id of ids) {
        clearTimeout(noticeTimersRef.current.get(id));
        noticeTimersRef.current.delete(id);
      }
      setRequested((cur) => {
        if (!ids.some((id) => cur.has(id))) return cur;
        const next = new Map(cur);
        ids.forEach((id) => next.delete(id));
        return next;
      });
    },
    [mountedRef],
  );

  // A start that shows in the head's status needs no notice any more, and a
  // head that is gone (deleted, or no head any more) none at all.
  useEffect(() => {
    const done = [...requested].filter(
      ([id, mark]) => !headIds.includes(id) || startVisible(specsById[id], mark),
    );
    drop(done.map(([id]) => id));
    // headIds is derived from specsById and mappings.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [requested, specsById, mappings, drop]);

  useEffect(() => {
    const timers = noticeTimersRef.current;
    return () => timers.forEach((timer) => clearTimeout(timer));
  }, []);

  return {
    requestStart: (id) => setRequested((cur) => new Map(cur).set(id, statusMark(specsById[id]))),
    // An accepted start has begun on the gateway: the notice, and with it the
    // fast cadence, lasts until the start shows in the head's status, or at
    // most CLUSTER_START_NOTICE_MS.
    settleStart: (id, accepted) => {
      if (!accepted) {
        drop([id]);
        return;
      }
      void refreshOne(id);
      clearTimeout(noticeTimersRef.current.get(id));
      noticeTimersRef.current.set(
        id,
        setTimeout(() => drop([id]), CLUSTER_START_NOTICE_MS),
      );
    },
    startPending: (id) => {
      const mark = requested.get(id);
      return mark !== undefined && !startVisible(specsById[id], mark);
    },
  };
}
