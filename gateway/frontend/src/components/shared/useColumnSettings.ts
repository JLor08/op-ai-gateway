// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useMemo } from 'react';
import { moveColumn, reconcileOrder, type DragPlace } from './columnDrag';
import { usePreference } from './preferences';

// Shared persisted column/tile settings, extracted from four structurally
// identical copies: Activity.tsx's table columns + stat tiles, ActivityGroups.tsx's
// metric columns, and shared/ListTable.tsx's columns. Each site had its own
// usePreference(hidden)/usePreference(order) pair, its own reconcile-against-
// catalogue memo, and its own toggle/reorder/reset trio wrapping moveColumn/
// reconcileOrder (shared/columnDrag.ts) — this hook wraps all of that once.

/**
 * Sanitize a persisted hidden-id set against a known-id catalogue: a
 * non-array/corrupt value (localStorage or profile corruption, or a foreign
 * value) falls back to `defaultHidden`; an array is filtered to ids present in
 * `catalogue` (unknown/stale/renamed ids dropped). Generic replacement for the
 * three structurally identical reconcileHidden{Columns,Tiles,GroupCols}
 * sanitizers that used to live next to each catalogue.
 */
export function reconcileHiddenIds<Id extends string>(
  parsed: unknown,
  catalogue: readonly Id[],
  defaultHidden: readonly Id[],
): Id[] {
  if (!Array.isArray(parsed)) return [...defaultHidden];
  const known = new Set<string>(catalogue);
  return parsed.filter((id): id is Id => typeof id === 'string' && known.has(id));
}

export type ColumnSettings<Id extends string> = {
  /** Reconciled order: known ids in persisted order, then any known ids not
   *  present (in catalogue order — so newly-added ids show up); unknown ids
   *  dropped. See reconcileOrder. */
  order: Id[];
  /** Reconciled hidden-id set: a non-array persisted value resets to
   *  `defaultHidden`; unknown ids are dropped. */
  hidden: Id[];
  /** `order` filtered to the ids not in `hidden` — the ids that should render,
   *  in render order. */
  visibleIds: Id[];
  /** Toggle one id's hidden state. */
  toggle: (id: string) => void;
  /** Move `sourceId` to sit immediately before/after `targetId` in `order`. */
  reorder: (sourceId: string, targetId: string, place: DragPlace) => void;
  /** Reset both hidden and order to `defaultHidden`/`catalogue`. */
  reset: () => void;
};

/**
 * Persisted column/tile settings: a usePreference pair (hidden set + order)
 * under `${baseKey}.hidden` / `${baseKey}.order`, each reconciled against
 * `catalogue` at read time, plus toggle/reorder/reset. `catalogue` doubles as
 * the default order (declaration order = default order, per every existing
 * call site); `defaultHidden` is the default-hidden subset of it.
 *
 * `catalogue`/`defaultHidden` should be referentially stable across renders
 * (a module-level constant, like the existing ACTIVITY_COLUMNS-derived arrays)
 * so the memos below — and usePreference's by-reference default (see
 * usePreference's own doc comment) — don't churn identity every render.
 */
export function useColumnSettings<Id extends string>(
  baseKey: string,
  catalogue: readonly Id[],
  defaultHidden: readonly Id[],
): ColumnSettings<Id> {
  const [storedHidden, setHidden] = usePreference<Id[]>(`${baseKey}.hidden`, defaultHidden as Id[]);
  const [storedOrder, setOrder] = usePreference<Id[]>(`${baseKey}.order`, catalogue as Id[]);

  const hidden = useMemo(
    () => reconcileHiddenIds(storedHidden, catalogue, defaultHidden),
    [storedHidden, catalogue, defaultHidden],
  );
  const order = useMemo(
    () => reconcileOrder(storedOrder, [...catalogue]) as Id[],
    [storedOrder, catalogue],
  );
  const visibleIds = useMemo(() => order.filter((id) => !hidden.includes(id)), [order, hidden]);

  const toggle = useCallback(
    (id: string) => {
      setHidden((cur) => {
        const base = reconcileHiddenIds(cur, catalogue, defaultHidden);
        return base.includes(id as Id) ? base.filter((x) => x !== id) : [...base, id as Id];
      });
    },
    [setHidden, catalogue, defaultHidden],
  );

  const reorder = useCallback(
    (sourceId: string, targetId: string, place: DragPlace) => {
      setOrder(
        (prev) =>
          moveColumn(reconcileOrder(prev, [...catalogue]), sourceId, targetId, place) as Id[],
      );
    },
    [setOrder, catalogue],
  );

  const reset = useCallback(() => {
    setHidden([...defaultHidden] as Id[]);
    setOrder([...catalogue] as Id[]);
  }, [setHidden, setOrder, defaultHidden, catalogue]);

  return { order, hidden, visibleIds, toggle, reorder, reset };
}
