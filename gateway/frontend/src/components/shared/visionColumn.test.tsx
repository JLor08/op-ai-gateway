// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { renderVisionCell, makeVisionColumn } from './visionColumn';
import { messages } from '../../i18n';

const t = messages.de;

afterEach(cleanup);

describe('renderVisionCell', () => {
  it('renders a neutral outlined chip with the given label when capable', () => {
    render(<>{renderVisionCell(true, 'Vision')}</>);
    const chip = screen.getByText('Vision').closest('.MuiChip-root')!;
    expect(chip).toHaveClass('MuiChip-outlined');
    // No color variant applied — a neutral chip, not success/error/etc.
    expect(chip.className).not.toMatch(
      /MuiChip-color(Success|Error|Warning|Info|Primary|Secondary)/,
    );
  });

  it('renders an en-dash (U+2013) when not capable', () => {
    render(<>{renderVisionCell(false, 'Vision')}</>);
    expect(screen.getByText('–')).toBeInTheDocument();
    expect(screen.queryByText('Vision')).toBeNull();
  });
});

describe('makeVisionColumn', () => {
  it('builds a ListColumn with the standard id/label/filter/enum shape', () => {
    const col = makeVisionColumn<{ v: boolean }>(t, (row) => row.v);
    expect(col.id).toBe('vision');
    expect(col.label).toBe(t.tableModelVision);
    expect(col.filter).toBe('enum');
    expect(col.searchable).toBe(false);
    expect(col.value({ v: true })).toBe('yes');
    expect(col.value({ v: false })).toBe('no');
    expect(col.enumLabel?.('yes')).toBe(t.tableModelVision);
    expect(col.enumLabel?.('no')).toBe('–');
    expect(col.defaultHidden).toBeUndefined();
  });

  it('sets defaultHidden when requested', () => {
    const col = makeVisionColumn<{ v: boolean }>(t, (row) => row.v, { defaultHidden: true });
    expect(col.defaultHidden).toBe(true);
  });
});
