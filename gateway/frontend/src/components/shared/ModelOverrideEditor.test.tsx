// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ModelOverrideEditor,
  buildOverrideMap,
  newOverrideRow,
  overrideRowsInvalid,
  overrideSummary,
  type OverrideRow,
  type OverrideRowValues,
} from './ModelOverrideEditor';
import { overrideTargets } from './OverrideTargetSelect';
import { messages, type Locale } from '../../i18n';

afterEach(cleanup);

// 'sd-child' is listed with flavors []: served under no API, so no picker
// offers it and a save that targets it is refused.
const targets = overrideTargets(
  [
    { id: 'qwen3-32b', display_name: 'qwen3-32b', flavors: ['openai'], loading_on_count: 0 },
    { id: 'sd-child', display_name: 'sd-child', flavors: [], loading_on_count: 0 },
  ],
  null,
);

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderEditor(opts: {
    rows: OverrideRowValues[];
    onRowsChange?: (rows: OverrideRow[]) => void;
    catchAll?: string;
    onCatchAllChange?: (value: string) => void;
  }) {
    render(
      <ModelOverrideEditor
        rows={opts.rows.map(newOverrideRow)}
        onRowsChange={opts.onRowsChange ?? vi.fn()}
        catchAll={opts.catchAll ?? ''}
        onCatchAllChange={opts.onCatchAllChange ?? vi.fn()}
        targets={targets}
        t={t}
        idPrefix="token"
        catchAllId="token-model-catchall"
      />,
    );
  }

  // The editor is fully controlled, so a regression about ROW IDENTITY needs a
  // real owner of the rows array — a vi.fn() would swallow the removal that the
  // bug depends on.
  function StatefulEditor({ initial }: Readonly<{ initial: OverrideRowValues[] }>) {
    const [rows, setRows] = useState<OverrideRow[]>(() => initial.map(newOverrideRow));
    const [catchAll, setCatchAll] = useState('');
    return (
      <ModelOverrideEditor
        rows={rows}
        onRowsChange={setRows}
        catchAll={catchAll}
        onCatchAllChange={setCatchAll}
        targets={targets}
        t={t}
        idPrefix="token"
        catchAllId="token-model-catchall"
      />
    );
  }

  describe(`buildOverrideMap [${locale}]`, () => {
    it('serialises both switches into the wire shape', () => {
      expect(
        buildOverrideMap([{ from: 'gpt-4o', to: 'qwen3-32b', offer: true, hideTarget: false }]),
      ).toEqual({ 'gpt-4o': { to: 'qwen3-32b', offer: true, hide_target: false } });
    });

    it('still drops incomplete rows', () => {
      expect(buildOverrideMap([{ from: '', to: 'x', offer: true, hideTarget: true }])).toEqual({});
    });
  });

  describe(`overrideRowsInvalid [${locale}]`, () => {
    // The two switches never make a row incomplete: validity is still purely
    // about exactly one of from/to being filled.
    it('ignores the switches; only from/to completeness matters', () => {
      expect(overrideRowsInvalid([{ from: 'a', to: '', offer: true, hideTarget: true }])).toBe(
        true,
      );
      expect(overrideRowsInvalid([{ from: '', to: '', offer: true, hideTarget: true }])).toBe(
        false,
      );
      expect(overrideRowsInvalid([{ from: 'a', to: 'b', offer: false, hideTarget: false }])).toBe(
        false,
      );
    });
  });

  describe(`overrideSummary [${locale}]`, () => {
    it('marks offered rows in the list summary', () => {
      expect(
        overrideSummary(t, {
          model_override: '',
          model_override_map: { 'gpt-4o': { to: 'qwen3-32b', offer: true, hide_target: false } },
        }),
      ).toContain('gpt-4o→qwen3-32b');
    });
  });

  describe(`ModelOverrideEditor row switches [${locale}]`, () => {
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
        expect.objectContaining({ from: 'a', to: 'x', offer: false, hideTarget: false }),
        expect.objectContaining({ from: 'b', to: 'y', offer: true, hideTarget: false }),
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
        expect.objectContaining({ from: 'a', to: 'x', offer: false, hideTarget: true }),
        expect.objectContaining({ from: 'b', to: 'y', offer: false, hideTarget: false }),
      ]);
    });

    it('adds a new row with both switches off', () => {
      const onRowsChange = vi.fn();
      renderEditor({ rows: [], onRowsChange });
      fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
      expect(onRowsChange).toHaveBeenCalledWith([
        expect.objectContaining({ from: '', to: '', offer: false, hideTarget: false }),
      ]);
    });

    it('gives every added row an id of its own', () => {
      const rows: OverrideRow[] = [];
      const onRowsChange = vi.fn((next: OverrideRow[]) => rows.push(...next));
      renderEditor({ rows: [], onRowsChange });
      fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
      fireEvent.click(screen.getByRole('button', { name: t.tokenOverrideAddRow }));
      expect(rows).toHaveLength(2);
      expect(rows[0].id).not.toEqual(rows[1].id);
    });
  });

  describe(`ModelOverrideEditor targets [${locale}]`, () => {
    it('offers a rule target only the names served under an API', async () => {
      renderEditor({ rows: [{ from: 'a', to: '', offer: false, hideTarget: false }] });
      fireEvent.mouseDown(screen.getByRole('combobox', { name: `${t.tokenOverrideToLabel} 1` }));
      expect(await screen.findByRole('option', { name: 'qwen3-32b' })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: /sd-child/ })).not.toBeInTheDocument();
    });

    it('offers the catch-all only the names served under an API', async () => {
      renderEditor({ rows: [] });
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.tokenOverrideCatchAllLabel }));
      expect(await screen.findByRole('option', { name: 'qwen3-32b' })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: /sd-child/ })).not.toBeInTheDocument();
    });

    it('keeps a refused rule target and catch-all visible and marks both unavailable', () => {
      renderEditor({
        rows: [{ from: 'a', to: 'sd-child', offer: false, hideTarget: false }],
        catchAll: 'sd-child',
      });
      const unavailable = `sd-child ${t.tokenOverrideTargetUnavailable}`;
      expect(screen.getByRole('combobox', { name: `${t.tokenOverrideToLabel} 1` })).toHaveValue(
        unavailable,
      );
      expect(screen.getByRole('combobox', { name: t.tokenOverrideCatchAllLabel })).toHaveValue(
        unavailable,
      );
      expect(screen.getAllByTestId('searchable-select-unavailable')).toHaveLength(2);
    });
  });

  describe(`ModelOverrideEditor row identity [${locale}]`, () => {
    // Regression (SonarQube typescript:S6479). The rows used to be keyed by their
    // array index. React keeps a row's mounted component instances per KEY, so an
    // index key means removing a row does not remove that row's instances: every
    // later row slides onto its predecessor's, and the last one is destroyed. The
    // values survive only because from/to/offer/hideTarget are all controlled —
    // everything React holds ON the instance does not, so the user's focus is
    // thrown away (asserted below) and the SearchableSelect's uncontrolled
    // Autocomplete input state lands on a different row than the one it was typed
    // into. A stable per-row id makes a removal remove exactly that row.
    it('leaves the surviving rows on their own instances when an earlier row is removed', () => {
      render(
        <StatefulEditor
          initial={[
            { from: 'a', to: 'qwen3-32b', offer: false, hideTarget: false },
            { from: 'b', to: 'qwen3-32b', offer: false, hideTarget: false },
            { from: 'c', to: 'qwen3-32b', offer: false, hideTarget: false },
          ]}
        />,
      );
      const offerSwitches = () => screen.getAllByRole('checkbox', { name: t.tokenOverrideOffer });

      // Switch the LAST row on and leave the focus there, as a user clicking it
      // would.
      const lastRowOffer = offerSwitches()[2];
      fireEvent.click(lastRowOffer);
      lastRowOffer.focus();
      expect(offerSwitches().map((c) => (c as HTMLInputElement).checked)).toEqual([
        false,
        false,
        true,
      ]);

      // Remove the FIRST row — the one the switched row is not.
      fireEvent.click(
        screen.getAllByRole('button', { name: new RegExp(t.tokenOverrideRemoveRow) })[0],
      );

      // Each remaining row still carries its own values...
      expect(screen.getAllByRole('textbox').map((i) => (i as HTMLInputElement).value)).toEqual([
        'b',
        'c',
      ]);
      expect(offerSwitches().map((c) => (c as HTMLInputElement).checked)).toEqual([false, true]);

      // ...on its own instance: the switched row is still THE SAME element, now
      // last of two, and it still has the focus. Both of these fail when the rows
      // are keyed by index — React then reuses the middle row's element for it and
      // unmounts this one, taking the focus with it.
      expect(offerSwitches()[1]).toBe(lastRowOffer);
      expect(document.activeElement).toBe(lastRowOffer);
    });
  });
}
