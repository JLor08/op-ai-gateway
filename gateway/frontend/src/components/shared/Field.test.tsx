// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Field } from './Field';

afterEach(cleanup);

// jsdom does no layout, so these tests CANNOT see the pixel-level defects the
// fixes address (a focus border struck through a label; a hover/focus outline
// on a read-only field). They pin the STRUCTURE the fixes add — the emitted
// notch-reset rule, the readonly attribute, the read-only marker — each of
// which is absent before the fix and present after. The real visual proof is a
// browser screenshot; see the reproduction notes, not these assertions.

/** All CSS emotion has injected into this document (jsdom, non-speedy). */
function emittedCss(): string {
  return Array.from(document.querySelectorAll('style'))
    .map((s) => s.textContent ?? '')
    .join('\n');
}

describe('Field', () => {
  it('wires label -> input id and forwards changes', () => {
    const onChange = vi.fn();
    render(<Field id="token-name" label="Name" value="" onChange={onChange} />);
    const input = screen.getByLabelText('Name');
    expect(input).toHaveAttribute('id', 'token-name');
    fireEvent.change(input, { target: { value: 'x' } });
    expect(onChange).toHaveBeenCalled();
  });

  it('supports a label-less input with an aria-label (inline edit)', () => {
    render(
      <Field
        id="edit-name"
        value="Dev Token"
        onChange={() => {}}
        inputProps={{ 'aria-label': 'Name Dev Token' }}
      />,
    );
    expect(screen.getByLabelText('Name Dev Token')).toHaveAttribute('id', 'edit-name');
  });

  // BUG 1 (notch opens in lockstep with focus). STRUCTURAL: a labelled field
  // renders a notched <legend> holding the label span (the notch machinery is
  // wired), and Field emits the rule that drops the legend's max-width
  // transition so the gap is open the instant the label rises. jsdom cannot
  // observe the ~50ms overlap itself; it can see that the reset rule exists.
  it('renders a notched legend carrying the label (multiline, size=small)', () => {
    const { container } = render(
      <Field id="args" label="Argumente" value="" onChange={() => {}} multiline minRows={3} />,
    );
    const legend = container.querySelector('fieldset legend');
    expect(legend).not.toBeNull();
    expect(legend?.querySelector('span')?.textContent).toBe('Argumente');
  });

  it('emits the notch transition reset so the gap opens in lockstep with focus', () => {
    // Descendant selector `.MuiOutlinedInput-notchedOutline legend` is uniquely
    // ours (MUI styles the legend via its own generated class, never this
    // combinator), so finding it with `transition:none` proves the sx applied.
    render(
      <Field id="args" label="Argumente" value="" onChange={() => {}} multiline minRows={3} />,
    );
    expect(emittedCss()).toMatch(
      /\.MuiOutlinedInput-notchedOutline legend\s*\{[^}]*transition:\s*none/,
    );
  });

  // BUG 2 (read-only reads as read-only). STRUCTURAL: the `readOnly` prop sets
  // the native readonly attribute AND stamps the root with the static-outline
  // marker the read-only sx keys off. jsdom cannot see that hover/focus no
  // longer recolour the outline; it can see the attribute and the marker, both
  // absent on an editable field.
  it('readOnly sets the native readonly attribute and the static-outline marker', () => {
    const { container } = render(
      <Field id="ro" label="Gateway-Modellname" value="gw-1" onChange={() => {}} readOnly />,
    );
    expect((screen.getByLabelText('Gateway-Modellname') as HTMLInputElement).readOnly).toBe(true);
    expect(container.querySelector('[data-readonly="true"]')).not.toBeNull();
  });

  it('an editable field carries neither the readonly attribute nor the marker', () => {
    const { container } = render(
      <Field id="rw" label="Gateway-Modellname" value="gw-1" onChange={() => {}} />,
    );
    expect((screen.getByLabelText('Gateway-Modellname') as HTMLInputElement).readOnly).toBe(false);
    expect(container.querySelector('[data-readonly="true"]')).toBeNull();
  });

  it('readOnly keeps any caller inputProps (e.g. an aria-label)', () => {
    render(
      <Field
        id="ro-aria"
        value="v"
        onChange={() => {}}
        readOnly
        inputProps={{ 'aria-label': 'App name' }}
      />,
    );
    expect((screen.getByLabelText('App name') as HTMLInputElement).readOnly).toBe(true);
  });
});
