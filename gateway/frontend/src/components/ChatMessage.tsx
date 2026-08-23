// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { memo, useState } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import rehypeHighlight from 'rehype-highlight';
import remarkGfm from 'remark-gfm';
import { Box, Button, IconButton, Stack, TextField, Tooltip, Typography } from '@mui/material';
import ReplayIcon from '@mui/icons-material/Replay';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import CodeIcon from '@mui/icons-material/Code';
import EditIcon from '@mui/icons-material/Edit';
import type { ChatContent } from './shared/chatContent';
import type { Translation } from './shared/types';

function contentText(content: ChatContent): string {
  if (typeof content === 'string') return content;
  return content
    .filter((part): part is { type: 'text'; text: string } => part.type === 'text')
    .map((part) => part.text)
    .join('\n');
}

function contentImages(content: ChatContent): string[] {
  if (typeof content === 'string') return [];
  return content
    .filter(
      (part): part is { type: 'image_url'; image_url: { url: string } } =>
        part.type === 'image_url',
    )
    .map((part) => part.image_url.url);
}

// Rendered markdown links always open in a new tab. A stable module-level
// component (rather than an inline arrow defined per-render) so ReactMarkdown
// doesn't see a new `a` component identity on every ChatMessage render.
const markdownLinkComponent: Components['a'] = (props) => (
  <a {...props} target="_blank" rel="noopener noreferrer" />
);

function ChatMessageComponent({
  t,
  role,
  content,
  reasoning,
  reasoningMs,
  streaming,
  ttftMs,
  tps,
  onEdit,
  onRegenerate,
  canRun = true,
}: Readonly<{
  t: Translation;
  role: 'user' | 'assistant';
  content: ChatContent;
  reasoning?: string;
  reasoningMs?: number;
  streaming?: boolean;
  ttftMs?: number;
  tps?: number;
  onEdit?: (text: string) => void;
  onRegenerate?: () => void;
  // Whether a new run may be started. False while the model is unavailable —
  // the Edit/Regenerate controls (which each start a run) are disabled to match
  // the blocked Send. Defaults true so callers that don't gate stay unchanged.
  canRun?: boolean;
}>) {
  const text = contentText(content);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(text);
  const [showRaw, setShowRaw] = useState(false);

  const roleLabel = role === 'user' ? t.userRole : t.assistantRole;

  if (role === 'assistant') {
    const showReasoning = Boolean(reasoning) || (streaming && text.length === 0);
    const reasoningText = reasoning ?? '';
    let summary: string;
    if (streaming && text.length === 0) {
      summary = `${t.chatReasoningActive} · ${reasoningText.length} ${t.chatCharsUnit}`;
    } else {
      const durationSuffix = reasoningMs ? `, ${(reasoningMs / 1000).toFixed(1)}s` : '';
      summary = `${t.chatReasoning} (${reasoningText.length} ${t.chatCharsUnit}${durationSuffix})`;
    }

    return (
      <Box
        component="article"
        data-role={role}
        sx={{
          // Answer bubbles hug their content, but never narrower than a request
          // bubble's fixed width (760px, responsive floor via min()) and never
          // wider than a bit under the chat column (90%). So a short reply is at
          // least as wide as a question, and a long reply grows toward the edge.
          width: 'fit-content',
          minWidth: 'min(760px, 100%)',
          maxWidth: '90%',
          mb: 2,
          // Gateway (assistant) answers are right-aligned: a block element capped
          // at maxWidth with ml:auto hugs the right edge; the accent stripe moves
          // to the right so it faces inward toward the conversation centre.
          ml: 'auto',
          p: '14px 16px',
          bgcolor: 'var(--surface)',
          borderRight: '4px solid var(--brand-primary)',
          borderRadius: '10px',
        }}
      >
        <Typography
          component="div"
          sx={{ fontSize: 13, textTransform: 'uppercase', fontWeight: 700 }}
        >
          {roleLabel}
        </Typography>
        {showReasoning && (
          <Box
            component="details"
            sx={{
              mt: 1.25,
              border: '1px solid var(--line)',
              bgcolor: 'var(--page)',
              borderRadius: '8px',
              color: 'var(--muted)',
              fontSize: 13,
              '& > summary': {
                cursor: 'pointer',
                p: '8px 12px',
                fontWeight: 700,
                listStyle: 'none',
              },
            }}
          >
            <summary>{summary}</summary>
            <Box
              sx={{
                p: '10px 12px',
                lineHeight: 1.5,
                whiteSpace: 'pre-wrap',
                overflowWrap: 'anywhere',
              }}
            >
              {reasoningText}
            </Box>
          </Box>
        )}
        <Box
          sx={{
            color: 'var(--text)',
            lineHeight: 1.55,
            overflowWrap: 'anywhere',
            '& > :first-of-type': { mt: 0 },
            '& > :last-child': { mb: 0 },
            '& p': { m: '0 0 10px' },
            '& ul, & ol': { m: '0 0 10px', pl: '22px' },
            '& li': { m: '2px 0' },
            '& li > p': { m: 0 },
            '& h1, & h2, & h3, & h4': { m: '16px 0 8px', lineHeight: 1.25 },
            '& h1': { fontSize: 22 },
            '& h2': { fontSize: 19 },
            '& h3': { fontSize: 16 },
            '& a': { color: 'var(--brand-primary)', textDecoration: 'underline' },
            '& blockquote': {
              m: '0 0 10px',
              p: '2px 14px',
              borderLeft: '3px solid var(--line)',
              color: 'var(--muted)',
            },
            '& hr': { border: 0, borderTop: '1px solid var(--line)', m: '14px 0' },
            '& img': { maxWidth: '100%', height: 'auto', borderRadius: '8px' },
            // Keep GFM tables contained in the bubble: block layout, no min-width,
            // horizontal scroll, and explicit cell borders/padding (the MUI Table
            // defaults do not apply to react-markdown's raw <table> output).
            '& table': {
              display: 'block',
              width: 'auto',
              minWidth: 0,
              maxWidth: '100%',
              overflowX: 'auto',
              borderCollapse: 'collapse',
              m: '0 0 12px',
            },
            '& th, & td': {
              border: '1px solid var(--line)',
              p: '6px 10px',
              fontSize: 14,
              fontWeight: 600,
              textAlign: 'left',
              textTransform: 'none',
            },
            '& th': { bgcolor: 'var(--page)', color: 'var(--text)' },
            '& pre': { overflowX: 'auto' },
            '& code': { fontFamily: 'monospace' },
            '& :not(pre) > code': {
              bgcolor: 'var(--accent-soft)',
              color: 'var(--brand-primary)',
              p: '1px 5px',
              borderRadius: '4px',
              fontSize: '0.9em',
            },
          }}
        >
          {showRaw ? (
            <Box
              component="pre"
              sx={{
                m: 0,
                whiteSpace: 'pre-wrap',
                overflowWrap: 'anywhere',
                fontFamily: 'monospace',
                fontSize: 13,
              }}
            >
              {text}
            </Box>
          ) : (
            <ReactMarkdown
              remarkPlugins={[remarkGfm]}
              rehypePlugins={[rehypeHighlight]}
              components={{ a: markdownLinkComponent }}
            >
              {text}
            </ReactMarkdown>
          )}
          {streaming && (
            <Box
              component="span"
              data-testid="chat-stream-cursor"
              sx={{
                display: 'inline-block',
                width: '8px',
                height: '1em',
                ml: '2px',
                bgcolor: 'var(--brand-primary)',
                verticalAlign: 'text-bottom',
              }}
            />
          )}
        </Box>
        <Stack direction="row" sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 0.5, mt: 1.25 }}>
          {onRegenerate && (
            <Tooltip title={t.chatRegenerate}>
              <span>
                <IconButton
                  size="small"
                  aria-label={t.chatRegenerate}
                  onClick={onRegenerate}
                  disabled={!canRun}
                >
                  <ReplayIcon fontSize="small" />
                </IconButton>
              </span>
            </Tooltip>
          )}
          <Tooltip title={t.chatCopy}>
            <IconButton
              size="small"
              aria-label={t.chatCopy}
              onClick={() => void navigator.clipboard?.writeText(text).catch(() => {})}
            >
              <ContentCopyIcon fontSize="small" />
            </IconButton>
          </Tooltip>
          <Tooltip title={showRaw ? t.chatViewRendered : t.chatViewRaw}>
            <IconButton
              size="small"
              aria-label={showRaw ? t.chatViewRendered : t.chatViewRaw}
              onClick={() => setShowRaw((v) => !v)}
            >
              <CodeIcon fontSize="small" />
            </IconButton>
          </Tooltip>
          {(ttftMs !== undefined || tps !== undefined) && (
            <Typography
              component="span"
              sx={{ ml: 1, color: 'var(--muted)', fontSize: 12, whiteSpace: 'nowrap' }}
            >
              {ttftMs !== undefined ? `${t.chatTtftLabel} ${(ttftMs / 1000).toFixed(1)}s` : ''}
              {ttftMs !== undefined && tps !== undefined ? ' · ' : ''}
              {tps !== undefined ? `${Math.round(tps)} ${t.chatCharsPerSecUnit}` : ''}
            </Typography>
          )}
        </Stack>
      </Box>
    );
  }

  const images = contentImages(content);

  return (
    <Box
      component="article"
      data-role={role}
      sx={{
        maxWidth: 760,
        mb: 2,
        p: '14px 16px',
        bgcolor: 'var(--accent-soft)',
        borderLeft: '4px solid var(--brand-accent)',
        borderRadius: '10px',
      }}
    >
      <Typography
        component="div"
        sx={{ fontSize: 13, textTransform: 'uppercase', fontWeight: 700 }}
      >
        {roleLabel}
      </Typography>
      {editing ? (
        <Box sx={{ display: 'grid', gap: 1 }}>
          <TextField
            multiline
            minRows={3}
            fullWidth
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            slotProps={{ htmlInput: { 'aria-label': t.messageLabel } }}
          />
          <Stack direction="row" sx={{ flexWrap: 'wrap', gap: 1, mt: 1.25 }}>
            <Button
              size="small"
              onClick={() => {
                onEdit?.(draft);
                setEditing(false);
              }}
            >
              {t.chatSave}
            </Button>
            <Button
              size="small"
              color="secondary"
              onClick={() => {
                setDraft(text);
                setEditing(false);
              }}
            >
              {t.chatCancel}
            </Button>
          </Stack>
        </Box>
      ) : (
        <>
          <Box>{text}</Box>
          {images.length > 0 && (
            <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, mb: 1 }}>
              {images.map((url, index) => (
                <Box
                  component="img"
                  key={index}
                  src={url}
                  alt={t.chatAttachedImage}
                  sx={{
                    display: 'block',
                    width: 72,
                    height: 72,
                    objectFit: 'cover',
                    border: '1px solid var(--line)',
                    borderRadius: '8px',
                  }}
                />
              ))}
            </Box>
          )}
          {onEdit && (
            <Stack direction="row" sx={{ flexWrap: 'wrap', gap: 1, mt: 1.25 }}>
              <Tooltip title={t.chatEdit}>
                <span>
                  <IconButton
                    size="small"
                    aria-label={t.chatEdit}
                    onClick={() => {
                      setDraft(text);
                      setEditing(true);
                    }}
                    disabled={!canRun}
                  >
                    <EditIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            </Stack>
          )}
        </>
      )}
    </Box>
  );
}

// Memoized so a streaming update (which only changes the last message's props)
// does not re-run react-markdown parsing for every earlier message. Relies on
// the Chat container keying messages by stable id and passing referentially
// stable onEdit/onRegenerate callbacks per message.
export const ChatMessage = memo(ChatMessageComponent);
