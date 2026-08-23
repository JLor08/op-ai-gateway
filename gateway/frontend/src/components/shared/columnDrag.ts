// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type DragEvent } from 'react';
import type { CSSObject } from '@mui/material';

// Shared, dependency-free column reordering for the new tables (client-side
// ListTable and the server-side ActivityTable). The reorder + reconcile logic
// is pure (and unit-tested); useColumnDrag wraps the HTML5 drag events; the two
// tables render their own headers and only spread the drag props + style hooks.

export type DragPlace = 'before' | 'after';

/**
 * Reconcile a persisted order against the current set of known columns:
 * keep persisted ids that still exist (in their persisted order), then append
 * any known ids not present (in default order — so newly-added columns show up),
 * and drop unknown/duplicate ids. Non-array input yields the default order.
 */
export function reconcileOrder(persisted: unknown, defaultOrder: string[]): string[] {
  const known = new Set(defaultOrder);
  const seen = new Set<string>();
  const out: string[] = [];
  if (Array.isArray(persisted)) {
    for (const id of persisted) {
      if (typeof id === 'string' && known.has(id) && !seen.has(id)) {
        out.push(id);
        seen.add(id);
      }
    }
  }
  for (const id of defaultOrder) {
    if (!seen.has(id)) {
      out.push(id);
      seen.add(id);
    }
  }
  return out;
}

/**
 * Move `sourceId` to sit immediately before/after `targetId`. Returns the input
 * unchanged when source === target or either id is missing (defensive). Works on
 * the FULL order array, so hidden columns interspersed between visible ones keep
 * their relative positions.
 */
export function moveColumn(
  order: string[],
  sourceId: string,
  targetId: string,
  place: DragPlace,
): string[] {
  if (sourceId === targetId) return order;
  if (!order.includes(sourceId) || !order.includes(targetId)) return order;
  const without = order.filter((id) => id !== sourceId);
  let index = without.indexOf(targetId);
  if (index === -1) return order;
  if (place === 'after') index += 1;
  without.splice(index, 0, sourceId);
  return without;
}

/**
 * Header drag-and-drop reordering. Instantiated inside the component that renders
 * the header cells; spread `dragProps(colId)` on each reorderable header cell and
 * feed `draggingId`/`overId`/`overPlace` into `columnDragSx` for the visual state.
 * `onReorder` receives (sourceId, targetId, place) and should apply `moveColumn`.
 */
export function useColumnDrag(
  onReorder: (sourceId: string, targetId: string, place: DragPlace) => void,
  orientation: 'horizontal' | 'vertical' = 'horizontal',
) {
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [overId, setOverId] = useState<string | null>(null);
  const [overPlace, setOverPlace] = useState<DragPlace>('before');

  function placeFromEvent(event: DragEvent<HTMLElement>): DragPlace {
    const rect = event.currentTarget.getBoundingClientRect();
    if (orientation === 'vertical')
      return event.clientY - rect.top < rect.height / 2 ? 'before' : 'after';
    return event.clientX - rect.left < rect.width / 2 ? 'before' : 'after';
  }

  function clear() {
    setDraggingId(null);
    setOverId(null);
  }

  function dragProps(colId: string) {
    return {
      draggable: true,
      onDragStart: (event: DragEvent<HTMLElement>) => {
        setDraggingId(colId);
        event.dataTransfer.effectAllowed = 'move';
        // Firefox refuses to start a drag unless some data is set. Use a custom
        // MIME type (NOT text/plain) so the payload can't be dropped into an
        // ordinary text input — e.g. overshooting a reorder onto the search box
        // would otherwise inject the column id as a query. We never read this
        // back (the drop handler uses draggingId state), so the value is inert.
        try {
          event.dataTransfer.setData('application/x-op-column', colId);
        } catch {
          /* ignore */
        }
      },
      onDragOver: (event: DragEvent<HTMLElement>) => {
        if (draggingId === null || draggingId === colId) return;
        event.preventDefault();
        event.dataTransfer.dropEffect = 'move';
        const place = placeFromEvent(event);
        if (overId !== colId || overPlace !== place) {
          setOverId(colId);
          setOverPlace(place);
        }
      },
      onDrop: (event: DragEvent<HTMLElement>) => {
        if (draggingId === null || draggingId === colId) {
          clear();
          return;
        }
        event.preventDefault();
        onReorder(draggingId, colId, placeFromEvent(event));
        clear();
      },
      onDragEnd: clear,
    };
  }

  return { dragProps, draggingId, overId, overPlace, clear };
}

/**
 * Visual state for a reorderable header cell: grab cursor, dimmed while it is the
 * one being dragged, and a colored insertion bar on the side it would drop into.
 */
export function columnDragSx(
  colId: string,
  draggingId: string | null,
  overId: string | null,
  overPlace: DragPlace,
  orientation: 'horizontal' | 'vertical' = 'horizontal',
): CSSObject {
  const isOver = overId === colId && draggingId !== null && draggingId !== colId;
  const barOffset = overPlace === 'before' ? '2px' : '-2px';
  const bar =
    orientation === 'vertical'
      ? `inset 0 ${barOffset} 0 0 var(--brand-primary)`
      : `inset ${barOffset} 0 0 0 var(--brand-primary)`;
  return {
    cursor: 'grab',
    userSelect: 'none',
    opacity: draggingId === colId ? 0.4 : 1,
    boxShadow: isOver ? bar : 'none',
  };
}
