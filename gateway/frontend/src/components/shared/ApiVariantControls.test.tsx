// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiVariantControls } from './ApiVariantControls';
import { messages } from '../../i18n';
import type { EndpointMode } from '../../api';

const t = messages.de;

afterEach(cleanup);

// A stateful harness so a checkbox toggle re-renders the controls with the new
// apiFlavors (the component is controlled; the parent owns the state).
function Harness({
  flavors = ['openai', 'anthropic'],
  responses = 'passthrough',
  msgs = 'passthrough',
}: {
  flavors?: string[];
  responses?: EndpointMode;
  msgs?: EndpointMode;
}) {
  const [apiFlavors, setApiFlavors] = useState<string[]>(flavors);
  const [responsesMode, setResponsesMode] = useState<EndpointMode>(responses);
  const [messagesMode, setMessagesMode] = useState<EndpointMode>(msgs);
  return (
    <ApiVariantControls
      t={t}
      apiFlavors={apiFlavors}
      responsesMode={responsesMode}
      messagesMode={messagesMode}
      onFlavorsChange={setApiFlavors}
      onResponsesModeChange={setResponsesMode}
      onMessagesModeChange={setMessagesMode}
    />
  );
}

const responsesField = () => screen.getByRole('combobox', { name: t.applicationResponsesMode });
const messagesField = () => screen.getByRole('combobox', { name: t.applicationMessagesMode });

describe('ApiVariantControls', () => {
  it('renders both flavor checkboxes and, when checked, enables the dropdowns showing the stored mode', () => {
    render(<Harness />);
    expect(screen.getByRole('checkbox', { name: 'openai' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: 'anthropic' })).toBeChecked();
    expect(responsesField()).not.toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModePassthrough);
    expect(messagesField()).toHaveTextContent(t.applicationModePassthrough);
  });

  it('disables the Codex dropdown and shows Deaktiviert when openai is unchecked, ignoring the stored mode', () => {
    render(<Harness flavors={['anthropic']} responses="translate" />);
    expect(responsesField()).toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeDisabled);
    // The anthropic side is unaffected.
    expect(messagesField()).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('restores the stored mode when the flavor is re-checked (mode not clobbered while unchecked)', () => {
    render(<Harness responses="translate" />);
    expect(responsesField()).toHaveTextContent(t.applicationModeTranslate);
    fireEvent.click(screen.getByRole('checkbox', { name: 'openai' })); // uncheck
    expect(responsesField()).toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeDisabled);
    fireEvent.click(screen.getByRole('checkbox', { name: 'openai' })); // re-check
    expect(responsesField()).not.toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeTranslate);
  });

  it('offers exactly the three modes in order and reports the chosen one', () => {
    render(<Harness />);
    fireEvent.mouseDown(messagesField());
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
      t.applicationModeDisabled,
      t.applicationModeTranslate,
      t.applicationModePassthrough,
    ]);
    fireEvent.click(screen.getByRole('option', { name: t.applicationModeTranslate }));
    expect(messagesField()).toHaveTextContent(t.applicationModeTranslate);
  });

  it('reports the toggled flavor list', () => {
    const onFlavorsChange = vi.fn();
    render(
      <ApiVariantControls
        t={t}
        apiFlavors={['openai', 'anthropic']}
        responsesMode="passthrough"
        messagesMode="passthrough"
        onFlavorsChange={onFlavorsChange}
        onResponsesModeChange={() => {}}
        onMessagesModeChange={() => {}}
      />,
    );
    fireEvent.click(screen.getByRole('checkbox', { name: 'anthropic' }));
    expect(onFlavorsChange).toHaveBeenCalledWith(['openai']);
  });
});
