// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ModelOverrideEditor,
  buildOverrideMap,
  overrideRowsInvalid,
  overrideSummary,
  type OverrideRow,
} from './ModelOverrideEditor';
import { messages } from '../../i18n';

const t = messages.de;

afterEach(cleanup);

const models = [{ id: 'qwen3-32b', display_name: 'qwen3-32b' }];

function renderEditor(opts: {
  rows: OverrideRow[];
  onRowsChange?: (rows: OverrideRow[]) => void;
  catchAll?: string;
  onCatchAllChange?: (value: string) => void;
}) {
  render(
    <ModelOverrideEditor
      rows={opts.rows}
      onRowsChange={opts.onRowsChange ?? vi.fn()}
      catchAll={opts.catchAll ?? ''}
      onCatchAllChange={opts.onCatchAllChange ?? vi.fn()}
      models={models}
      t={t}
      idPrefix="token"
      catchAllId="token-model-catchall"
    />,
  );
}

describe('buildOverrideMap', () => {
  it('serialises both switches into the wire shape', () => {
    expect(
      buildOverrideMap([{ from: 'gpt-4o', to: 'qwen3-32b', offer: true, hideTarget: false }]),
    ).toEqual({ 'gpt-4o': { to: 'qwen3-32b', offer: true, hide_target: false } });
  });

  it('still drops incomplete rows', () => {
    expect(buildOverrideMap([{ from: '', to: 'x', offer: true, hideTarget: true }])).toEqual({});
  });
});

describe('overrideRowsInvalid', () => {
  // The two switches never make a row incomplete: validity is still purely
  // about exactly one of from/to being filled.
  it('ignores the switches; only from/to completeness matters', () => {
    expect(overrideRowsInvalid([{ from: 'a', to: '', offer: true, hideTarget: true }])).toBe(true);
    expect(overrideRowsInvalid([{ from: '', to: '', offer: true, hideTarget: true }])).toBe(false);
    expect(overrideRowsInvalid([{ from: 'a', to: 'b', offer: false, hideTarget: false }])).toBe(
      false,
    );
  });
});

describe('overrideSummary', () => {
  it('marks offered rows in the list summary', () => {
    expect(
      overrideSummary(t, {
        model_override: '',
        model_override_map: { 'gpt-4o': { to: 'qwen3-32b', offer: true, hide_target: false } },
      }),
    ).toContain('gpt-4o→qwen3-32b');
  });
});

describe('ModelOverrideEditor row switches', () => {
  it('toggles the offer switch of one row only', async () => {
    // Two rows share one editor; a toggle that leaked to the sibling would be
    // invisible in a single-row test.
    const onRowsChange = vi.fn();
    renderEditor({
      rows: [
        { from: 'a', to: 'x', offer: false, hideTarget: false },
        { from: 'b', to: 'y', offer: false, hideTarget: false },
      ],
      onRowsChange,
    });
    fireEvent.click(screen.getAllByRole('checkbox', { name: t.tokenOverrideOffer })[1]);
    expect(onRowsChange).toHaveBeenCalledWith([
      { from: 'a', to: 'x', offer: false, hideTarget: false },
      { from: 'b', to: 'y', offer: true, hideTarget: false },
    ]);
  });

  it('toggles the hide-target switch of one row only', () => {
    const onRowsChange = vi.fn();
    renderEditor({
      rows: [
        { from: 'a', to: 'x', offer: false, hideTarget: false },
        { from: 'b', to: 'y', offer: false, hideTarget: false },
      ],
      onRowsChange,
    });
    fireEvent.click(screen.getAllByRole('checkbox', { name: t.tokenOverrideHideTarget })[0]);
    expect(onRowsChange).toHaveBeenCalledWith([
      { from: 'a', to: 'x', offer: false, hideTarget: true },
      { from: 'b', to: 'y', offer: false, hideTarget: false },
    ]);
  });

  it('adds a new row with both switches off', () => {
    const onRowsChange = vi.fn();
    renderEditor({ rows: [], onRowsChange });
    fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
    expect(onRowsChange).toHaveBeenCalledWith([
      { from: '', to: '', offer: false, hideTarget: false },
    ]);
  });
});
