// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ChatSidebar } from './ChatSidebar';
import { ChatStoreContext, type ChatStore } from './ChatStore';
import { messages } from '../../i18n';

const t = messages.de;

// A minimal fake store injected via the context — the sidebar only reads the
// multi-chat surface + the actions, so the streaming/settings fields can be
// bare defaults.
function makeStore(overrides: Partial<ChatStore> = {}): ChatStore {
  return {
    messages: [],
    streaming: false,
    runningChatIds: new Set(),
    isChatRunning: () => false,
    input: '',
    images: [],
    model: '',
    systemPrompt: '',
    temperature: 1,
    maxTokens: 0,
    selectedTokenId: '',
    chatModels: [],
    modelOptions: [],
    usableTokens: [],
    manageableServers: [],
    serverOverride: '',
    serverOverrideForceUnreachable: false,
    setServerOverride: vi.fn(),
    setServerOverrideForceUnreachable: vi.fn(),
    serverOverrideLocksChat: false,
    effectiveServerOverride: '',
    effectiveServerOverrideForce: false,
    serverOverrideModels: [],
    overrideModel: '',
    overrideLocksModel: false,
    modelAvailable: true,
    modelVisionCapable: true,
    chats: [
      { id: 'c1', title: 'First chat', created_at: '', updated_at: '2026-07-17T13:00:00Z' },
      { id: 'c2', title: '', created_at: '', updated_at: '2026-07-17T12:00:00Z' },
    ],
    activeChatId: 'c1',
    chatsLoading: false,
    selectChat: vi.fn(),
    deleteChat: vi.fn(),
    renameChat: vi.fn(),
    send: vi.fn(),
    stop: vi.fn(),
    newChat: vi.fn(),
    setInput: vi.fn(),
    handleFiles: vi.fn(async () => {}),
    removeImage: vi.fn(),
    setModel: vi.fn(),
    setSystemPrompt: vi.fn(),
    setTemperature: vi.fn(),
    setMaxTokens: vi.fn(),
    setSelectedTokenId: vi.fn(),
    handlersFor: vi.fn(() => ({ onEdit: () => {}, onRegenerate: () => {} })),
    ...overrides,
  };
}

function renderSidebar(store: ChatStore, collapsed = false, onToggle = vi.fn()) {
  render(
    <ChatStoreContext.Provider value={store}>
      <ChatSidebar t={t} collapsed={collapsed} onToggleCollapse={onToggle} />
    </ChatStoreContext.Provider>,
  );
  return { onToggle };
}

afterEach(cleanup);

describe('ChatSidebar', () => {
  it('renders the chat list (untitled rows fall back to the placeholder)', () => {
    renderSidebar(makeStore());
    expect(screen.getByText('First chat')).toBeInTheDocument();
    // The empty-title chat shows the localized placeholder.
    expect(screen.getByText(t.chatUntitled)).toBeInTheDocument();
  });

  it('shows a loading state while chatsLoading', () => {
    renderSidebar(makeStore({ chatsLoading: true, chats: [] }));
    expect(screen.getByRole('status')).toHaveTextContent(t.loading);
  });

  it('shows the empty state when there are no chats', () => {
    renderSidebar(makeStore({ chats: [], activeChatId: null }));
    expect(screen.getByText(t.chatListEmpty)).toBeInTheDocument();
  });

  it("calls newChat when 'Neuer Chat' is clicked", () => {
    const store = makeStore();
    renderSidebar(store);
    fireEvent.click(screen.getByRole('button', { name: t.chatNewChat }));
    expect(store.newChat).toHaveBeenCalledTimes(1);
  });

  it('calls selectChat when a row is clicked', () => {
    const store = makeStore();
    renderSidebar(store);
    fireEvent.click(screen.getByRole('button', { name: t.chatUntitled }));
    expect(store.selectChat).toHaveBeenCalledWith('c2');
  });

  it('deletes a chat after confirming', () => {
    const store = makeStore();
    renderSidebar(store);
    // Open the confirm dialog from the first row's delete action.
    fireEvent.click(screen.getAllByRole('button', { name: t.chatDelete })[0]);
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(t.chatDeleteConfirm)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: t.chatDelete }));
    expect(store.deleteChat).toHaveBeenCalledWith('c1');
  });

  it('copies the chat id to the clipboard from the copy action', () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    renderSidebar(makeStore());
    // The copy action is the first row action; click it on the first row (c1).
    fireEvent.click(screen.getAllByRole('button', { name: t.chatCopyId })[0]);
    expect(writeText).toHaveBeenCalledWith('c1');
  });

  it('renames a chat inline on double-click + Enter', async () => {
    const store = makeStore();
    renderSidebar(store);
    fireEvent.doubleClick(screen.getByRole('button', { name: 'First chat' }));
    const input = await screen.findByRole('textbox', { name: t.chatRename });
    fireEvent.change(input, { target: { value: 'Renamed title' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(store.renameChat).toHaveBeenCalledWith('c1', 'Renamed title'));
  });

  it('shows a running indicator only for a chat the store reports as running', () => {
    const store = makeStore({
      runningChatIds: new Set(['c2']),
      isChatRunning: (id) => id === 'c2',
    });
    renderSidebar(store);
    expect(screen.queryByTestId('chat-running-c1')).not.toBeInTheDocument();
    expect(screen.getByTestId('chat-running-c2')).toBeInTheDocument();
  });

  // Regression (review Minor C): rename/delete are gated per ROW by that row's
  // running state, not by the active chat's streaming flag. A running row's
  // buttons disable; an idle row stays actionable even while another row runs.
  it('disables rename/delete only for the running row (per-row gating)', () => {
    // c1 (active) is running; c2 is idle. streaming stays false to prove the
    // gating keys off the per-row running state, not the store's streaming flag.
    const store = makeStore({ streaming: false, isChatRunning: (id) => id === 'c1' });
    renderSidebar(store);
    const renameButtons = screen.getAllByRole('button', { name: t.chatRename });
    const deleteButtons = screen.getAllByRole('button', { name: t.chatDelete });
    // Row c1 (running) is locked...
    expect(renameButtons[0]).toBeDisabled();
    expect(deleteButtons[0]).toBeDisabled();
    // ...while row c2 (idle) is still actionable.
    expect(renameButtons[1]).not.toBeDisabled();
    expect(deleteButtons[1]).not.toBeDisabled();
  });

  // The three row actions sit in an absolutely-positioned overlay above the row,
  // where they used to cover the chat title. They stay transparent (and
  // click-through) until the row is hovered or something inside it takes focus.
  //
  // jsdom never matches :hover, so the reveal itself is not assertable here —
  // this pins the resting state, which is the half that was broken.
  it('keeps the row actions transparent while the row is at rest', () => {
    renderSidebar(makeStore());
    const actions = screen.getAllByTestId('chat-row-actions')[0];
    expect(getComputedStyle(actions).opacity).toBe('0');
    expect(getComputedStyle(actions).pointerEvents).toBe('none');
  });

  // jsdom cannot enter :hover, so the assertion above would still pass if the
  // reveal selector were misspelled — leaving the actions invisible forever with
  // a green suite. This checks the emitted rules exist and turn the actions back
  // on, for the pointer case and the keyboard case.
  it('emits reveal rules for hover and focus-within', () => {
    renderSidebar(makeStore());
    const actionsClass = screen
      .getAllByTestId('chat-row-actions')[0]
      .className.split(' ')
      .find((name) => name.startsWith('op-chat-row-actions'));
    expect(actionsClass).toBeDefined();

    // Two traps to avoid. A substring check would also accept
    // `.op-chat-row-actions-typo`, which selects nothing — so match the class as
    // a whole token. And a comma-joined rule must be split into its individual
    // selectors, or one intact half vouches for a broken other half.
    const targetsActions = new RegExp(`\\.${actionsClass}(?![\\w-])`);
    const revealing = [...document.styleSheets]
      .flatMap((sheet) => [...sheet.cssRules])
      .filter((rule): rule is CSSStyleRule => rule instanceof CSSStyleRule)
      .filter((rule) => rule.style.opacity === '1')
      .flatMap((rule) => (rule.selectorText ?? '').split(','))
      .map((selector) => selector.trim())
      .filter((selector) => targetsActions.test(selector));

    expect(revealing.some((selector) => selector.includes(':hover'))).toBe(true);
    expect(revealing.some((selector) => selector.includes(':focus-within'))).toBe(true);

    // A pointer-less device never enters :hover, so the actions must be on
    // unconditionally there or they are unreachable.
    const touchFallback = [...document.styleSheets]
      .flatMap((sheet) => [...sheet.cssRules])
      .filter((rule): rule is CSSMediaRule => rule instanceof CSSMediaRule)
      .filter((rule) => rule.conditionText.replace(/\s/g, '').includes('hover:none'))
      .flatMap((rule) => [...rule.cssRules])
      .filter((rule): rule is CSSStyleRule => rule instanceof CSSStyleRule)
      .filter((rule) => rule.style.opacity === '1')
      .flatMap((rule) => (rule.selectorText ?? '').split(','))
      .map((selector) => selector.trim())
      .filter((selector) => targetsActions.test(selector));

    expect(touchFallback).not.toHaveLength(0);
  });

  // The icons are allowed to sit above the title, so the title fades out
  // underneath them. Same reasoning as the test above: jsdom cannot hover, but a
  // selector that stopped matching would silently bring the tangle back.
  it('emits a fade rule for the title under the revealed actions', () => {
    renderSidebar(makeStore());
    const titleClass = screen
      .getByText('First chat')
      .className.split(' ')
      .find((name) => name.startsWith('op-chat-row-title'));
    expect(titleClass).toBeDefined();

    const targetsTitle = new RegExp(`\\.${titleClass}(?![\\w-])`);
    const masking = [...document.styleSheets]
      .flatMap((sheet) => [...sheet.cssRules])
      .filter((rule): rule is CSSStyleRule => rule instanceof CSSStyleRule)
      .filter((rule) => rule.style.getPropertyValue('mask-image').includes('linear-gradient'))
      .flatMap((rule) => (rule.selectorText ?? '').split(','))
      .map((selector) => selector.trim())
      .filter((selector) => targetsTitle.test(selector));

    expect(masking.some((selector) => selector.includes(':hover'))).toBe(true);
    expect(masking.some((selector) => selector.includes(':focus-within'))).toBe(true);
  });

  it('collapses to a rail: hides the list and exposes an expand button', () => {
    const store = makeStore();
    renderSidebar(store, true);
    expect(screen.queryByText('First chat')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.chatExpandList })).toBeInTheDocument();
  });

  it('invokes onToggleCollapse from the collapse toggle', () => {
    const store = makeStore();
    const { onToggle } = renderSidebar(store, false);
    fireEvent.click(screen.getByRole('button', { name: t.chatCollapseList }));
    expect(onToggle).toHaveBeenCalledTimes(1);
  });
});
