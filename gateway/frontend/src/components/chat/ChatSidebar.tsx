// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type KeyboardEvent, type ReactNode } from 'react';
import {
  Box,
  Button,
  List,
  ListItem,
  ListItemButton,
  ListItemText,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import ContentCopyIcon from '@mui/icons-material/ContentCopyOutlined';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import type { Translation } from '../shared/types';
import { IconAction } from '../shared/IconAction';
import { ConfirmDialog } from '../shared/ConfirmDialog';
import { useChatStore } from './ChatStore';

/**
 * Collapsible left rail listing the user's persistent chats. Mirrors the
 * NavSidebar width-transition pattern (`var(--sidebar)` theming, icon-only when
 * collapsed). Reads the multi-chat surface directly from the store; the parent
 * owns only the collapsed flag (persisted in localStorage).
 */
export function ChatSidebar({
  t,
  collapsed,
  onToggleCollapse,
}: Readonly<{
  t: Translation;
  collapsed: boolean;
  onToggleCollapse: () => void;
}>) {
  const c = useChatStore();
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState('');
  const [pendingDeleteId, setPendingDeleteId] = useState<string | null>(null);

  function startRename(id: string, currentTitle: string) {
    setRenamingId(id);
    setRenameValue(currentTitle);
  }

  function commitRename() {
    if (renamingId) {
      const trimmed = renameValue.trim();
      if (trimmed) c.renameChat(renamingId, trimmed);
    }
    setRenamingId(null);
    setRenameValue('');
  }

  function cancelRename() {
    setRenamingId(null);
    setRenameValue('');
  }

  function onRenameKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key === 'Enter') {
      event.preventDefault();
      commitRename();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      cancelRename();
    }
  }

  if (collapsed) {
    return (
      <Box
        component="nav"
        aria-label={t.chatSidebarLabel}
        sx={{
          flex: '0 0 auto',
          width: 56,
          transition: 'width 180ms ease',
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          gap: 1,
          py: 1.5,
          bgcolor: 'var(--sidebar)',
          border: '1px solid var(--line)',
          borderRadius: '10px',
        }}
      >
        <IconAction
          label={t.chatExpandList}
          icon={<ChevronRightIcon fontSize="small" />}
          onClick={onToggleCollapse}
        />
        <IconAction label={t.chatNewChat} icon={<AddIcon fontSize="small" />} onClick={c.newChat} />
      </Box>
    );
  }

  let chatListBody: ReactNode;
  if (c.chatsLoading) {
    chatListBody = (
      <Typography role="status" sx={{ px: 1.5, py: 1, color: 'var(--muted)' }}>
        {t.loading}
      </Typography>
    );
  } else if (c.chats.length === 0) {
    chatListBody = (
      <Typography sx={{ px: 1.5, py: 1, color: 'var(--muted)' }}>{t.chatListEmpty}</Typography>
    );
  } else {
    chatListBody = (
      <List disablePadding>
        {c.chats.map((chat) => {
          const isActive = chat.id === c.activeChatId;
          const title = chat.title.trim() || t.chatUntitled;
          if (renamingId === chat.id) {
            return (
              <ListItem key={chat.id} disablePadding sx={{ px: 1, py: 0.5 }}>
                <TextField
                  autoFocus
                  fullWidth
                  size="small"
                  slotProps={{ htmlInput: { 'aria-label': t.chatRename } }}
                  value={renameValue}
                  onChange={(event) => setRenameValue(event.target.value)}
                  onKeyDown={onRenameKeyDown}
                  onBlur={commitRename}
                />
              </ListItem>
            );
          }
          const isRunning = c.isChatRunning(chat.id);
          return (
            <ListItem
              key={chat.id}
              disablePadding
              secondaryAction={
                <Stack direction="row" sx={{ gap: 0 }}>
                  <IconAction
                    label={t.chatCopyId}
                    icon={<ContentCopyIcon fontSize="small" />}
                    onClick={() => void navigator.clipboard?.writeText(chat.id).catch(() => {})}
                  />
                  <IconAction
                    label={t.chatRename}
                    icon={<EditOutlinedIcon fontSize="small" />}
                    onClick={() => startRename(chat.id, chat.title)}
                    disabled={isRunning}
                  />
                  <IconAction
                    label={t.chatDelete}
                    icon={<DeleteOutlineIcon fontSize="small" />}
                    color="error"
                    onClick={() => setPendingDeleteId(chat.id)}
                    disabled={isRunning}
                  />
                </Stack>
              }
            >
              <ListItemButton
                selected={isActive}
                aria-current={isActive ? 'true' : undefined}
                onClick={() => c.selectChat(chat.id)}
                onDoubleClick={() => !isRunning && startRename(chat.id, chat.title)}
                sx={{
                  borderRadius: '8px',
                  mx: 0.5,
                  pr: 13,
                  gap: 1,
                  color: 'var(--nav-text)',
                  '&.Mui-selected, &.Mui-selected:hover': {
                    bgcolor: 'var(--sidebar-active)',
                    color: 'var(--nav-active-text)',
                  },
                }}
              >
                <ListItemText
                  primary={title}
                  slotProps={{
                    primary: {
                      sx: {
                        fontWeight: isActive ? 700 : 500,
                        whiteSpace: 'nowrap',
                        overflow: 'hidden',
                        textOverflow: 'ellipsis',
                      },
                    },
                  }}
                />
                {isRunning && (
                  <Box
                    component="span"
                    data-testid={`chat-running-${chat.id}`}
                    aria-hidden="true"
                    sx={{
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
            </ListItem>
          );
        })}
      </List>
    );
  }

  return (
    <Box
      component="nav"
      aria-label={t.chatSidebarLabel}
      sx={{
        flex: '0 0 auto',
        width: 260,
        transition: 'width 180ms ease',
        display: 'flex',
        flexDirection: 'column',
        minHeight: 0,
        bgcolor: 'var(--sidebar)',
        border: '1px solid var(--line)',
        borderRadius: '10px',
        overflow: 'hidden',
      }}
    >
      <Stack
        direction="row"
        sx={{
          alignItems: 'center',
          justifyContent: 'space-between',
          px: 1.5,
          py: 1,
          borderBottom: '1px solid var(--line)',
        }}
      >
        <Typography component="h2" sx={{ fontWeight: 700, fontSize: 16, color: 'var(--nav-text)' }}>
          {t.chatSidebarLabel}
        </Typography>
        <IconAction
          label={t.chatCollapseList}
          icon={<ChevronLeftIcon fontSize="small" />}
          onClick={onToggleCollapse}
        />
      </Stack>

      <Box sx={{ p: 1.5 }}>
        <Button
          fullWidth
          variant="outlined"
          size="small"
          startIcon={<AddIcon fontSize="small" />}
          onClick={c.newChat}
        >
          {t.chatNewChat}
        </Button>
      </Box>

      <Box sx={{ flex: 1, minHeight: 0, overflowY: 'auto', px: 0.5, pb: 1 }}>{chatListBody}</Box>

      <ConfirmDialog
        open={pendingDeleteId !== null}
        title={t.chatDeleteConfirm}
        confirmLabel={t.chatDelete}
        cancelLabel={t.chatCancel}
        onConfirm={() => {
          if (pendingDeleteId) c.deleteChat(pendingDeleteId);
          setPendingDeleteId(null);
        }}
        onCancel={() => setPendingDeleteId(null)}
      />
    </Box>
  );
}
