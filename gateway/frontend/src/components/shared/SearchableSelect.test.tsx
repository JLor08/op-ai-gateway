// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, within } from '@testing-library/react';
import { SearchableSelect, matchOptions } from './SearchableSelect';

const OPTIONS = [
  { value: '', label: 'Ohne Token (Session)' },
  { value: 'gpt-oss-20b', label: 'gpt-oss-20b' },
  { value: 'qwen-coder', label: 'qwen-coder' },
];

afterEach(() => cleanup());

// The type-to-filter logic is unit-tested directly (MUI Autocomplete's input can't
// be driven by fireEvent without user-event, which the repo does not depend on).
describe('matchOptions (search filter)', () => {
  it('shows ALL options while the input still equals the selected label', () => {
    expect(matchOptions(OPTIONS, 'gpt-oss-20b', 'gpt-oss-20b')).toEqual(OPTIONS);
  });

  it('filters by a case-insensitive substring once a query is typed', () => {
    expect(matchOptions(OPTIONS, 'QWEN', 'gpt-oss-20b').map((o) => o.value)).toEqual([
      'qwen-coder',
    ]);
    expect(matchOptions(OPTIONS, 'oss', null).map((o) => o.value)).toEqual(['gpt-oss-20b']);
  });

  it('returns all options for an empty query and none for a non-match', () => {
    expect(matchOptions(OPTIONS, '', null)).toEqual(OPTIONS);
    expect(matchOptions(OPTIONS, 'zzz', null)).toEqual([]);
  });
});

function renderSelect(value = 'gpt-oss-20b', onChange = vi.fn()) {
  render(
    <SearchableSelect id="s" label="Modell" value={value} onChange={onChange} options={OPTIONS} />,
  );
  return { onChange, input: screen.getByRole('combobox', { name: 'Modell' }) as HTMLInputElement };
}

describe('SearchableSelect', () => {
  it("shows the selected option's label in the input", () => {
    const { input } = renderSelect('qwen-coder');
    expect(input.value).toBe('qwen-coder');
  });

  it('shows ALL options when opened (before the user types a query)', () => {
    const { input } = renderSelect('gpt-oss-20b');
    fireEvent.mouseDown(input);
    fireEvent.click(input);
    const opts = within(screen.getByRole('listbox'))
      .getAllByRole('option')
      .map((o) => o.textContent);
    expect(opts).toEqual(['Ohne Token (Session)', 'gpt-oss-20b', 'qwen-coder']);
  });

  it("calls onChange with the picked option's value", () => {
    const { input, onChange } = renderSelect('gpt-oss-20b');
    fireEvent.mouseDown(input);
    fireEvent.click(input);
    fireEvent.click(screen.getByRole('option', { name: 'qwen-coder' }));
    expect(onChange).toHaveBeenCalledWith('qwen-coder');
  });

  it('selecting the empty option yields the empty value', () => {
    const { input, onChange } = renderSelect('gpt-oss-20b');
    fireEvent.mouseDown(input);
    fireEvent.click(input);
    fireEvent.click(screen.getByRole('option', { name: 'Ohne Token (Session)' }));
    expect(onChange).toHaveBeenCalledWith('');
  });

  it('disables the input when disabled', () => {
    render(
      <SearchableSelect
        id="s"
        label="Modell"
        value=""
        onChange={vi.fn()}
        options={OPTIONS}
        disabled
      />,
    );
    expect(screen.getByRole('combobox', { name: 'Modell' })).toBeDisabled();
  });

  it('renders a loaded marker (with its title) on a loaded option', () => {
    const options = [
      { value: 'a', label: 'model-a', loaded: true, loadedTitle: 'Geladen auf: GPU-Box' },
      { value: 'b', label: 'model-b' },
    ];
    render(
      <SearchableSelect id="s" label="Modell" value="a" onChange={vi.fn()} options={options} />,
    );
    const input = screen.getByRole('combobox', { name: 'Modell' });
    fireEvent.mouseDown(input);
    fireEvent.click(input);
    const listbox = screen.getByRole('listbox');
    // The loaded option carries its title on the marker span; the other does not.
    expect(within(listbox).getByTitle('Geladen auf: GPU-Box')).toBeInTheDocument();
    expect(
      within(listbox)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['model-a', 'model-b']);
  });

  it('renders a long option on a single line and in FULL (no truncation)', () => {
    const longLabel = 'a-very-long-model-name-that-should-not-be-truncated-in-the-dropdown';
    render(
      <SearchableSelect
        id="s"
        label="Modell"
        value="a"
        onChange={vi.fn()}
        options={[{ value: 'a', label: longLabel }]}
      />,
    );
    const input = screen.getByRole('combobox', { name: 'Modell' });
    fireEvent.mouseDown(input);
    fireEvent.click(input);
    const option = within(screen.getByRole('listbox')).getByRole('option');
    // Single line…
    expect(option).toHaveStyle({ whiteSpace: 'nowrap' });
    // …and the WHOLE label is present (no ellipsis truncation).
    expect(option.textContent).toBe(longLabel);
    // The label span itself must not clip with an ellipsis.
    const labelSpan = option.querySelector('span:last-child') as HTMLElement;
    expect(labelSpan.style.textOverflow).not.toBe('ellipsis');
  });

  it('carries the full selected label as the input title (so it stays readable when clipped)', () => {
    const longLabel = 'another-really-long-selected-model-name-xyz';
    render(
      <SearchableSelect
        id="s"
        label="Modell"
        value="a"
        onChange={vi.fn()}
        options={[{ value: 'a', label: longLabel }]}
      />,
    );
    const input = screen.getByRole('combobox', { name: 'Modell' });
    expect(input).toHaveAttribute('title', longLabel);
  });

  it('shows a leading green loaded dot in the field for a loaded selected option', () => {
    render(
      <SearchableSelect
        id="s"
        label="Modell"
        value="a"
        onChange={vi.fn()}
        options={[
          { value: 'a', label: 'model-a', loaded: true, loadedTitle: 'Geladen auf: GPU-1' },
        ]}
      />,
    );
    expect(screen.getByTestId('searchable-select-loaded-dot')).toBeInTheDocument();
    expect(screen.queryByTestId('searchable-select-unavailable')).not.toBeInTheDocument();
  });

  it('shows a leading red warning when unavailable, taking precedence over the loaded dot', () => {
    render(
      <SearchableSelect
        id="s"
        label="Modell"
        value="a"
        onChange={vi.fn()}
        unavailable
        unavailableTitle="Modell nicht verfügbar"
        options={[{ value: 'a', label: 'model-a', loaded: true, loadedTitle: 'Geladen' }]}
      />,
    );
    expect(screen.getByTestId('searchable-select-unavailable')).toBeInTheDocument();
    // The unavailable warning replaces the loaded dot (an unavailable model can't be loaded).
    expect(screen.queryByTestId('searchable-select-loaded-dot')).not.toBeInTheDocument();
  });
});
