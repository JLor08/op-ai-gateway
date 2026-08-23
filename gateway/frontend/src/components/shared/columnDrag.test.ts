// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import type { DragEvent as ReactDragEvent } from 'react';
import { reconcileOrder, moveColumn, useColumnDrag, columnDragSx } from './columnDrag';

describe('reconcileOrder', () => {
  const def = ['a', 'b', 'c', 'd'];

  it('returns the default order for non-array input', () => {
    expect(reconcileOrder(null, def)).toEqual(def);
    expect(reconcileOrder(undefined, def)).toEqual(def);
    expect(reconcileOrder('nope', def)).toEqual(def);
    expect(reconcileOrder({}, def)).toEqual(def);
    // A fresh copy, not the same reference.
    expect(reconcileOrder(null, def)).not.toBe(def);
  });

  it('keeps persisted known ids in their persisted order', () => {
    expect(reconcileOrder(['c', 'a', 'b', 'd'], def)).toEqual(['c', 'a', 'b', 'd']);
  });

  it('appends known ids missing from the persisted order (new columns) in default order', () => {
    expect(reconcileOrder(['c', 'a'], def)).toEqual(['c', 'a', 'b', 'd']);
  });

  it('drops unknown ids and de-duplicates', () => {
    expect(reconcileOrder(['c', 'zzz', 'a', 'a', 'c'], def)).toEqual(['c', 'a', 'b', 'd']);
  });

  it('ignores non-string entries', () => {
    expect(reconcileOrder(['b', 5, null, 'a'], def)).toEqual(['b', 'a', 'c', 'd']);
  });
});

describe('moveColumn', () => {
  const order = ['a', 'b', 'c', 'd'];

  it('is a no-op when source === target', () => {
    expect(moveColumn(order, 'b', 'b', 'before')).toBe(order);
  });

  it('is a no-op when an id is missing', () => {
    expect(moveColumn(order, 'x', 'b', 'before')).toBe(order);
    expect(moveColumn(order, 'b', 'x', 'after')).toBe(order);
  });

  it('moves a column before a target (rightward source)', () => {
    // move d before b
    expect(moveColumn(order, 'd', 'b', 'before')).toEqual(['a', 'd', 'b', 'c']);
  });

  it('moves a column after a target (rightward source)', () => {
    // move a after c
    expect(moveColumn(order, 'a', 'c', 'after')).toEqual(['b', 'c', 'a', 'd']);
  });

  it('swaps adjacent neighbours (the ←/→ menu button semantics)', () => {
    // move b before its left neighbour a
    expect(moveColumn(order, 'b', 'a', 'before')).toEqual(['b', 'a', 'c', 'd']);
    // move c after its right neighbour d
    expect(moveColumn(order, 'c', 'd', 'after')).toEqual(['a', 'b', 'd', 'c']);
  });

  it('moves a column to the very end', () => {
    expect(moveColumn(order, 'a', 'd', 'after')).toEqual(['b', 'c', 'd', 'a']);
  });

  it('does not mutate the input array', () => {
    const input = ['a', 'b', 'c'];
    moveColumn(input, 'a', 'c', 'after');
    expect(input).toEqual(['a', 'b', 'c']);
  });
});

describe('columnDragSx', () => {
  it('dims the column currently being dragged', () => {
    expect(columnDragSx('a', 'a', null, 'before').opacity).toBe(0.4);
    expect(columnDragSx('a', 'b', null, 'before').opacity).toBe(1);
  });

  it('has a grab cursor and disables text selection', () => {
    const sx = columnDragSx('a', null, null, 'before');
    expect(sx.cursor).toBe('grab');
    expect(sx.userSelect).toBe('none');
  });

  it('shows no insertion bar when the column is not the drop target', () => {
    expect(columnDragSx('a', 'b', 'c', 'before').boxShadow).toBe('none');
    // Not over anything.
    expect(columnDragSx('a', 'b', null, 'before').boxShadow).toBe('none');
    // Over itself does not draw a bar.
    expect(columnDragSx('a', 'a', 'a', 'before').boxShadow).toBe('none');
  });

  it('draws a left/right bar for a horizontal drop target', () => {
    expect(columnDragSx('b', 'a', 'b', 'before', 'horizontal').boxShadow).toContain(
      'inset 2px 0 0 0',
    );
    expect(columnDragSx('b', 'a', 'b', 'after', 'horizontal').boxShadow).toContain(
      'inset -2px 0 0 0',
    );
  });

  it('draws a top/bottom bar for a vertical drop target', () => {
    expect(columnDragSx('b', 'a', 'b', 'before', 'vertical').boxShadow).toContain(
      'inset 0 2px 0 0',
    );
    expect(columnDragSx('b', 'a', 'b', 'after', 'vertical').boxShadow).toContain(
      'inset 0 -2px 0 0',
    );
  });
});

// A minimal fake drag event whose currentTarget reports a fixed 100x20 box.
function fakeDragEvent(clientX: number, clientY: number) {
  return {
    currentTarget: { getBoundingClientRect: () => ({ left: 0, top: 0, width: 100, height: 20 }) },
    clientX,
    clientY,
    preventDefault: vi.fn(),
    dataTransfer: { effectAllowed: '', dropEffect: '', setData: vi.fn() },
  } as unknown as ReactDragEvent<HTMLElement>;
}

describe('useColumnDrag', () => {
  it('tracks the dragged column and reports before/after from the pointer position (horizontal)', () => {
    const onReorder = vi.fn();
    const { result } = renderHook(() => useColumnDrag(onReorder, 'horizontal'));

    act(() => result.current.dragProps('a').onDragStart(fakeDragEvent(5, 5)));
    expect(result.current.draggingId).toBe('a');

    // Pointer in the left half of the target -> "before".
    act(() => result.current.dragProps('b').onDragOver(fakeDragEvent(10, 5)));
    expect(result.current.overId).toBe('b');
    expect(result.current.overPlace).toBe('before');

    // Pointer in the right half -> "after".
    act(() => result.current.dragProps('b').onDragOver(fakeDragEvent(90, 5)));
    expect(result.current.overPlace).toBe('after');

    act(() => result.current.dragProps('b').onDrop(fakeDragEvent(90, 5)));
    expect(onReorder).toHaveBeenCalledWith('a', 'b', 'after');
    // State clears after the drop.
    expect(result.current.draggingId).toBeNull();
    expect(result.current.overId).toBeNull();
  });

  it('uses the vertical axis for before/after when oriented vertically', () => {
    const onReorder = vi.fn();
    const { result } = renderHook(() => useColumnDrag(onReorder, 'vertical'));
    act(() => result.current.dragProps('a').onDragStart(fakeDragEvent(5, 2)));
    // clientY in the top half (height 20) -> before.
    act(() => result.current.dragProps('b').onDragOver(fakeDragEvent(5, 3)));
    expect(result.current.overPlace).toBe('before');
    // clientY in the bottom half -> after.
    act(() => result.current.dragProps('b').onDragOver(fakeDragEvent(5, 17)));
    expect(result.current.overPlace).toBe('after');
    act(() => result.current.dragProps('b').onDrop(fakeDragEvent(5, 17)));
    expect(onReorder).toHaveBeenCalledWith('a', 'b', 'after');
  });

  it('does not reorder when dropping a column on itself', () => {
    const onReorder = vi.fn();
    const { result } = renderHook(() => useColumnDrag(onReorder));
    act(() => result.current.dragProps('a').onDragStart(fakeDragEvent(5, 5)));
    act(() => result.current.dragProps('a').onDrop(fakeDragEvent(5, 5)));
    expect(onReorder).not.toHaveBeenCalled();
  });

  it('clears the dragging state on dragend', () => {
    const onReorder = vi.fn();
    const { result } = renderHook(() => useColumnDrag(onReorder));
    act(() => result.current.dragProps('a').onDragStart(fakeDragEvent(5, 5)));
    expect(result.current.draggingId).toBe('a');
    act(() => result.current.dragProps('a').onDragEnd());
    expect(result.current.draggingId).toBeNull();
  });

  it('ignores dragover before any drag has started', () => {
    const onReorder = vi.fn();
    const { result } = renderHook(() => useColumnDrag(onReorder));
    const evt = fakeDragEvent(10, 5);
    act(() => result.current.dragProps('b').onDragOver(evt));
    expect(evt.preventDefault).not.toHaveBeenCalled();
    expect(result.current.overId).toBeNull();
  });
});
