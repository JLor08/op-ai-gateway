// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import ReactMarkdown, { type Components } from 'react-markdown';
import rehypeHighlight from 'rehype-highlight';
import remarkGfm from 'remark-gfm';
import { Box, type SxProps, type Theme } from '@mui/material';

// Rendered markdown links always open in a new tab. A stable module-level
// component (rather than an inline arrow defined per-render) so ReactMarkdown
// doesn't see a new `a` component identity on every render.
const markdownLinkComponent: Components['a'] = (props) => (
  <a {...props} target="_blank" rel="noopener noreferrer" />
);

const answerBodySx: SxProps<Theme> = {
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
};

// One run of an assistant answer's text as the bubble shows it: rendered markdown, or the raw
// source with `raw`, plus the stream cursor while the answer is still arriving. A voice answer
// with spoken sections is several of these with a collapsed block between each (SpokenAnswer);
// every other answer is exactly one.
export function AnswerBody({
  text,
  raw,
  streaming = false,
}: Readonly<{ text: string; raw: boolean; streaming?: boolean }>) {
  return (
    <Box sx={answerBodySx}>
      {raw ? (
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
  );
}
