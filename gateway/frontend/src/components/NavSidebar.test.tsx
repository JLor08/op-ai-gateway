// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  LayoutDashboard,
  MessageSquare,
  Network,
  ScrollText,
  SlidersHorizontal,
  Wrench,
} from 'lucide-react';
import { NavSidebar } from './NavSidebar';
import { ChatStreamingContext } from './chat/ChatStore';
import { messages } from '../i18n';
import type { CurrentUser } from '../api';
import type { NavItem } from './shared/types';

const t = messages.de;
const navItems: NavItem[] = [
  { id: 'dashboard', labelKey: 'dashboard', href: '/dashboard', icon: LayoutDashboard },
  { id: 'chat', labelKey: 'chat', href: '/chat', icon: MessageSquare },
  { id: 'tools', labelKey: 'tools', href: '/tools', icon: Wrench },
  { id: 'system', labelKey: 'system', href: '/system', icon: SlidersHorizontal },
  { id: 'netbird', labelKey: 'settingsNetbirdTitle', href: '/netbird', icon: Network },
  { id: 'logs', labelKey: 'logsNav', href: '/logs', icon: ScrollText },
];
const asUser = (role: string) => ({ role }) as unknown as CurrentUser;

afterEach(() => {
  cleanup();
});

describe('NavSidebar', () => {
  it('shows labels when expanded and marks the active view', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('user')}
        expanded
        t={t}
      />,
    );
    expect(screen.getByRole('link', { name: t.chat })).toBeInTheDocument();
    expect(screen.getByText(t.chat)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: t.dashboard })).toHaveAttribute('aria-current', 'page');
  });

  it('keeps links reachable by name when collapsed but hides the visible label', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('user')}
        expanded={false}
        t={t}
      />,
    );
    expect(screen.getByRole('link', { name: t.chat })).toBeInTheDocument();
    expect(screen.queryByText(t.chat)).not.toBeInTheDocument();
  });

  it('hides the system and logs items from non system-admins and fires onSelect', () => {
    const onSelect = vi.fn();
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={onSelect}
        currentUser={asUser('user')}
        expanded
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.system })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: t.logsNav })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('link', { name: t.chat }));
    expect(onSelect).toHaveBeenCalledWith('chat');
  });

  it('also hides the logs item from a plain admin (system-admin only)', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('admin')}
        expanded
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.logsNav })).not.toBeInTheDocument();
  });

  it('hides the system and logs items from a NOT-YET-ELEVATED system admin', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.system })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: t.logsNav })).not.toBeInTheDocument();
  });

  it('shows the system and logs items for an ELEVATED system admin', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        systemAdminMode
        t={t}
      />,
    );
    expect(screen.getByRole('link', { name: t.system })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: t.logsNav })).toBeInTheDocument();
  });

  it('shows the elevated System-Admin-Modus hint above Dashboard, only when elevated', () => {
    // Not elevated -> no hint.
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        t={t}
      />,
    );
    expect(screen.queryByText(t.systemAdminModeActive)).not.toBeInTheDocument();
    cleanup();

    // Elevated -> hint present, rendered ABOVE the Dashboard nav item.
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        systemAdminMode
        t={t}
      />,
    );
    const hint = screen.getByText(t.systemAdminModeActive);
    const dashboard = screen.getByRole('link', { name: t.dashboard });
    expect(hint.compareDocumentPosition(dashboard) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('renders the elevated hint as a labelled icon (no visible text) when collapsed', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded={false}
        systemAdminMode
        t={t}
      />,
    );
    expect(screen.queryByText(t.systemAdminModeActive)).not.toBeInTheDocument();
    expect(screen.getByRole('img', { name: t.systemAdminModeActive })).toBeInTheDocument();
  });

  it('shows Tools for admin and system_admin but hides it for a plain user', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('user')}
        expanded
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.tools })).not.toBeInTheDocument();
    cleanup();

    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('admin')}
        expanded
        t={t}
      />,
    );
    expect(screen.getByRole('link', { name: t.tools })).toBeInTheDocument();
    cleanup();

    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        t={t}
      />,
    );
    expect(screen.getByRole('link', { name: t.tools })).toBeInTheDocument();
  });

  it('shows NetBird only for a system_admin AND netbirdModuleEnabled; hidden otherwise', () => {
    // Flag on, but not a system_admin → hidden for both admin and user.
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('admin')}
        expanded
        netbirdModuleEnabled
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.settingsNetbirdTitle })).not.toBeInTheDocument();
    cleanup();

    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('user')}
        expanded
        netbirdModuleEnabled
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.settingsNetbirdTitle })).not.toBeInTheDocument();
    cleanup();

    // system_admin, ELEVATED, but the module flag is off → hidden.
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        systemAdminMode
        netbirdModuleEnabled={false}
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.settingsNetbirdTitle })).not.toBeInTheDocument();
    cleanup();

    // system_admin but NOT elevated, flag on → hidden (elevation required).
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        netbirdModuleEnabled
        t={t}
      />,
    );
    expect(screen.queryByRole('link', { name: t.settingsNetbirdTitle })).not.toBeInTheDocument();
    cleanup();

    // system_admin, ELEVATED, AND the flag on → visible.
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        systemAdminMode
        netbirdModuleEnabled
        t={t}
      />,
    );
    expect(screen.getByRole('link', { name: t.settingsNetbirdTitle })).toBeInTheDocument();
  });

  it('never renders a Policies item and orders Tools before System, NetBird after System', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('system_admin')}
        expanded
        systemAdminMode
        netbirdModuleEnabled
        t={t}
      />,
    );
    expect(screen.queryByText('Policies')).not.toBeInTheDocument();
    const links = screen.getAllByRole('link').map((el) => el.getAttribute('aria-label'));
    const toolsIdx = links.indexOf(t.tools);
    const systemIdx = links.indexOf(t.system);
    const netbirdIdx = links.indexOf(t.settingsNetbirdTitle);
    expect(toolsIdx).toBeGreaterThanOrEqual(0);
    expect(systemIdx).toBeGreaterThan(toolsIdx);
    expect(netbirdIdx).toBeGreaterThan(systemIdx);
  });

  it('does not show the chat streaming indicator without a provider (default false)', () => {
    render(
      <NavSidebar
        navItems={navItems}
        view="dashboard"
        onSelect={vi.fn()}
        currentUser={asUser('user')}
        expanded
        t={t}
      />,
    );
    expect(screen.queryByTestId('chat-streaming')).not.toBeInTheDocument();
    // aria-label stays the plain label (no running-state suffix).
    expect(screen.getByRole('link', { name: t.chat })).toBeInTheDocument();
  });

  it('shows the chat streaming indicator when a background stream is running', () => {
    render(
      <ChatStreamingContext.Provider value={true}>
        <NavSidebar
          navItems={navItems}
          view="dashboard"
          onSelect={vi.fn()}
          currentUser={asUser('user')}
          expanded
          t={t}
        />
      </ChatStreamingContext.Provider>,
    );
    expect(screen.getByTestId('chat-streaming')).toBeInTheDocument();
    // the running state is appended to the chat item's accessible name
    expect(
      screen.getByRole('link', { name: `${t.chat} · ${t.chatStreamingIndicator}` }),
    ).toBeInTheDocument();
    // other items are unaffected
    expect(screen.getByRole('link', { name: t.dashboard })).toBeInTheDocument();
  });

  it('keeps the chat item plain when the provider is not streaming', () => {
    render(
      <ChatStreamingContext.Provider value={false}>
        <NavSidebar
          navItems={navItems}
          view="dashboard"
          onSelect={vi.fn()}
          currentUser={asUser('user')}
          expanded
          t={t}
        />
      </ChatStreamingContext.Provider>,
    );
    expect(screen.queryByTestId('chat-streaming')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: t.chat })).toBeInTheDocument();
  });
});
