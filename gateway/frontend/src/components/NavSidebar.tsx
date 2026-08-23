// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Fragment } from 'react';
import { Box, ListItemButton, ListItemIcon, ListItemText, Tooltip } from '@mui/material';
import AdminPanelSettingsIcon from '@mui/icons-material/AdminPanelSettings';
import type { CurrentUser } from '../api';
import type { NavItem, Translation, View } from './shared/types';
import { useChatStreaming } from './chat/ChatStore';
import { viewRegistry, type ViewGateCtx } from './views';

export function NavSidebar({
  navItems,
  view,
  onSelect,
  currentUser,
  expanded,
  netbirdModuleEnabled = false,
  certificatesModuleEnabled = false,
  systemAdminMode = false,
  t,
}: Readonly<{
  navItems: NavItem[];
  view: View;
  onSelect: (v: View) => void;
  currentUser: CurrentUser | null;
  expanded: boolean;
  netbirdModuleEnabled?: boolean;
  // Gates the "certificates" nav item the same way netbirdModuleEnabled gates
  // "netbird" (raw module-enabled checkbox, so the item appears as soon as it's
  // flipped on, before the rest of the module is configured).
  certificatesModuleEnabled?: boolean;
  // System-admin step-up mode: a system_admin session starts as a plain admin
  // (system scope withheld) until it elevates. The system/netbird/certificates/
  // logs items require the ELEVATED capability, not just the role -- see App.tsx.
  systemAdminMode?: boolean;
  t: Translation;
}>) {
  // Safe outside a ChatStoreProvider (returns false); when a background chat
  // stream is running it flips true so the chat item shows a live indicator
  // even while the user is on another view.
  const chatStreaming = useChatStreaming();
  const role = currentUser?.role;
  const isAdmin = role === 'admin' || role === 'system_admin';
  // Same gate the routed content uses (viewRegistry) — nav visibility and
  // content visibility can never diverge. See views.tsx.
  const gateCtx: ViewGateCtx = {
    isAdmin,
    systemAdminMode,
    netbirdModuleEnabled,
    certificatesModuleEnabled,
  };
  const visible = navItems.filter((item) => viewRegistry[item.id].gate(gateCtx));

  return (
    <Box
      component="nav"
      aria-label={t.portalNavigation}
      sx={{
        flex: '0 0 auto',
        width: expanded ? 264 : 72,
        transition: 'width 180ms ease',
        overflowX: 'hidden',
        overflowY: 'auto',
        minHeight: 0,
        py: 1.5,
        bgcolor: 'var(--sidebar)',
        borderRight: '1px solid var(--line)',
      }}
    >
      {/* Prominent "elevated" hint above the first nav item (Dashboard). Shown
          only while in System-Admin mode; the enter/leave actions live in the
          user dropdown. It sits in the normal nav flow, so on short windows the
          menu items scroll beneath it and it never overlaps them. */}
      {systemAdminMode &&
        (expanded ? (
          <Box
            role="status"
            sx={{
              mx: '10px',
              mb: 1,
              px: 1.25,
              py: 0.75,
              display: 'flex',
              alignItems: 'center',
              gap: 1,
              borderRadius: 1,
              bgcolor: 'warning.main',
              color: 'warning.contrastText',
              fontWeight: 700,
              fontSize: 13,
              lineHeight: 1.3,
            }}
          >
            <AdminPanelSettingsIcon fontSize="small" aria-hidden="true" />
            {t.systemAdminModeActive}
          </Box>
        ) : (
          <Box sx={{ display: 'flex', justifyContent: 'center', mb: 1 }}>
            <Tooltip title={t.systemAdminModeActive} placement="right">
              <Box
                component="span"
                role="img"
                aria-label={t.systemAdminModeActive}
                sx={{
                  display: 'inline-flex',
                  p: 0.75,
                  borderRadius: 1,
                  bgcolor: 'warning.main',
                  color: 'warning.contrastText',
                }}
              >
                <AdminPanelSettingsIcon fontSize="small" aria-hidden="true" />
              </Box>
            </Tooltip>
          </Box>
        ))}
      {visible.map((item) => {
        const Icon = item.icon;
        const label = t[item.labelKey];
        const isActive = item.id === view;
        const isStreaming = item.id === 'chat' && chatStreaming;
        const ariaLabel = isStreaming ? `${label} · ${t.chatStreamingIndicator}` : label;
        const button = (
          <ListItemButton
            component="a"
            href={item.href}
            selected={isActive}
            aria-current={isActive ? 'page' : undefined}
            aria-label={ariaLabel}
            onClick={(event) => {
              event.preventDefault();
              onSelect(item.id);
            }}
            sx={{
              minHeight: 54,
              gap: 1.5,
              px: '14px',
              justifyContent: expanded ? 'flex-start' : 'center',
              color: 'var(--nav-text)',
              fontWeight: 700,
              borderLeft: '4px solid transparent',
              '&:hover': { bgcolor: 'var(--sidebar-active)' },
              '&.Mui-selected, &.Mui-selected:hover': {
                bgcolor: 'var(--sidebar-active)',
                borderLeftColor: 'var(--brand-primary)',
                color: 'var(--nav-active-text)',
              },
            }}
          >
            <ListItemIcon sx={{ minWidth: 0, color: 'inherit' }}>
              <Icon aria-hidden="true" size={19} strokeWidth={2.1} />
            </ListItemIcon>
            {expanded && (
              <ListItemText
                primary={label}
                slotProps={{ primary: { sx: { fontSize: 17, fontWeight: 700 } } }}
              />
            )}
            {isStreaming && (
              <Box
                component="span"
                data-testid="chat-streaming"
                aria-hidden="true"
                sx={{
                  ml: expanded ? 'auto' : 0.75,
                  flex: '0 0 auto',
                  width: 8,
                  height: 8,
                  borderRadius: '50%',
                  bgcolor: 'var(--brand-primary)',
                  animation: 'op-chat-pulse 1.4s ease-in-out infinite',
                  '@keyframes op-chat-pulse': {
                    '0%, 100%': { opacity: 1 },
                    '50%': { opacity: 0.35 },
                  },
                }}
              />
            )}
          </ListItemButton>
        );
        return expanded ? (
          <Fragment key={item.id}>{button}</Fragment>
        ) : (
          <Tooltip key={item.id} title={label} placement="right">
            {button}
          </Tooltip>
        );
      })}
    </Box>
  );
}
