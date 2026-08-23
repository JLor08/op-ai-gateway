// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  IconButton,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material';
import ExpandMore from '@mui/icons-material/ExpandMore';
import { Copy } from 'lucide-react';
import type { CaptureDetail } from '../api';
import type { Translation } from './shared/types';

function prettyJSON(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function extractResponsesText(obj: unknown): string {
  const output = (obj as { output?: unknown }).output;
  if (!Array.isArray(output)) {
    const flat = (obj as { output_text?: unknown }).output_text;
    return typeof flat === 'string' ? flat : '';
  }
  let text = '';
  for (const item of output) {
    const content = (item as { content?: unknown }).content;
    if (!Array.isArray(content)) continue;
    for (const part of content) {
      const value = (part as { text?: unknown }).text;
      if (typeof value === 'string') text += value;
    }
  }
  return text;
}

// Whether any line of `body`, once its leading whitespace is stripped, is an
// SSE `data:` frame. Avoids a `^\s*data:`-shaped regex (flagged by Sonar as
// super-linear on adversarial input) in favor of a plain per-line string check
// with identical semantics.
function hasStreamingDataLines(body: string): boolean {
  return body.split('\n').some((line) => line.trimStart().startsWith('data:'));
}

// Flavor-aware assistant-text extraction from the stored raw response body.
// Streaming OpenAI chunks are detected structurally (data: lines) and take
// precedence over the flavor switch. Exported for unit testing.
export function deriveChat(apiFlavor: string, body: string): string {
  if (hasStreamingDataLines(body)) {
    let out = '';
    for (const line of body.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed.startsWith('data:')) continue;
      const payload = trimmed.slice('data:'.length).trim();
      if (payload === '' || payload === '[DONE]') continue;
      try {
        const obj = JSON.parse(payload) as { choices?: Array<{ delta?: { content?: unknown } }> };
        const delta = obj.choices?.[0]?.delta?.content;
        if (typeof delta === 'string') out += delta;
      } catch {
        /* skip a non-JSON frame */
      }
    }
    return out;
  }
  let obj: unknown;
  try {
    obj = JSON.parse(body);
  } catch {
    return '';
  }
  switch (apiFlavor) {
    case 'openai_chat_completions': {
      const content = (obj as { choices?: Array<{ message?: { content?: unknown } }> }).choices?.[0]
        ?.message?.content;
      return typeof content === 'string' ? content : '';
    }
    case 'openai_responses':
      return extractResponsesText(obj);
    case 'anthropic_messages': {
      const parts = (obj as { content?: Array<{ text?: unknown }> }).content;
      if (!Array.isArray(parts)) return '';
      return parts.map((p) => (typeof p.text === 'string' ? p.text : '')).join('');
    }
    case 'portal_chat': {
      const content = (obj as { message?: { content?: unknown } }).message?.content;
      return typeof content === 'string' ? content : '';
    }
    default:
      return '';
  }
}

// Flavor-aware reasoning ("thinking") extraction from the stored raw response
// body, mirroring deriveChat: streaming SSE chunks take precedence over the
// flavor switch. Empty when no reasoning is present. Exported for unit testing.
export function deriveReasoning(apiFlavor: string, body: string): string {
  if (hasStreamingDataLines(body)) {
    let out = '';
    for (const line of body.split('\n')) {
      const trimmed = line.trim();
      if (!trimmed.startsWith('data:')) continue;
      const payload = trimmed.slice('data:'.length).trim();
      if (payload === '' || payload === '[DONE]') continue;
      try {
        const obj = JSON.parse(payload) as {
          choices?: Array<{ delta?: { reasoning_content?: unknown; reasoning?: unknown } }>;
        };
        const delta = obj.choices?.[0]?.delta;
        const value = delta?.reasoning_content ?? delta?.reasoning;
        if (typeof value === 'string') out += value;
      } catch {
        /* skip a non-JSON frame */
      }
    }
    return out;
  }
  let obj: unknown;
  try {
    obj = JSON.parse(body);
  } catch {
    return '';
  }
  switch (apiFlavor) {
    case 'openai_chat_completions': {
      const message = (
        obj as {
          choices?: Array<{ message?: { reasoning_content?: unknown; reasoning?: unknown } }>;
        }
      ).choices?.[0]?.message;
      const value = message?.reasoning_content ?? message?.reasoning;
      return typeof value === 'string' ? value : '';
    }
    case 'anthropic_messages': {
      const parts = (
        obj as { content?: Array<{ type?: unknown; thinking?: unknown; reasoning?: unknown }> }
      ).content;
      if (!Array.isArray(parts)) return '';
      return parts
        .filter((p) => p.type === 'thinking')
        .map((p) => {
          const value = p.thinking ?? p.reasoning;
          return typeof value === 'string' ? value : '';
        })
        .join('');
    }
    default:
      return '';
  }
}

function HeaderTable({
  t,
  headers,
}: Readonly<{ t: Translation; headers: Record<string, string[]> }>) {
  const entries = Object.entries(headers ?? {});
  return (
    <Table size="small">
      <TableHead>
        <TableRow>
          <TableCell>{t.tableName}</TableCell>
          <TableCell>{t.captureHeaderValue}</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {entries.map(([name, values]) => (
          <TableRow key={name}>
            <TableCell>{name}</TableCell>
            <TableCell sx={{ wordBreak: 'break-all' }}>{values.join(', ')}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function CopyButton({ t, text }: Readonly<{ t: Translation; text: string }>) {
  return (
    <Tooltip title={t.captureCopy}>
      <IconButton
        size="small"
        aria-label={t.captureCopy}
        onClick={() => void navigator.clipboard?.writeText(text).catch(() => {})}
      >
        <Copy size={16} />
      </IconButton>
    </Tooltip>
  );
}

const preSx = {
  m: 0,
  p: 1,
  overflowX: 'auto',
  bgcolor: 'action.hover',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-word',
} as const;

export function CaptureDialog({
  t,
  open,
  onClose,
  detail,
  loading,
  error,
  onRequestDelete,
  onToggleSecret,
}: Readonly<{
  t: Translation;
  open: boolean;
  onClose: () => void;
  detail: CaptureDetail | null;
  loading: boolean;
  error: string;
  onRequestDelete?: () => void;
  onToggleSecret?: (next: boolean) => void;
}>) {
  const [reqView, setReqView] = useState<'pretty' | 'raw'>('pretty');
  const [respView, setRespView] = useState<'chat' | 'raw'>('chat');

  let reqBody = '';
  if (detail) {
    reqBody = reqView === 'pretty' ? prettyJSON(detail.req_body) : detail.req_body;
  }
  // Chat view splits the assistant turn into reasoning ("Denken") + output
  // ("Ausgabe"); raw view shows the untouched body. The Copy button copies the
  // output text in chat view (parity with deriveChat) and the raw body in raw.
  const respReasoning =
    detail && respView === 'chat' ? deriveReasoning(detail.api_flavor, detail.resp_body) : '';
  const respOutput = detail ? deriveChat(detail.api_flavor, detail.resp_body) : '';
  let respBody = '';
  if (detail) {
    respBody = respView === 'chat' ? respOutput : detail.resp_body;
  }

  return (
    <Dialog open={open} onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle>{t.captureDialogTitle}</DialogTitle>
      <DialogContent dividers>
        {loading && <Typography>{t.loading}</Typography>}
        {error && (
          <Typography role="alert" color="error">
            {error}
          </Typography>
        )}
        {detail && (
          <Stack spacing={2}>
            <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
              <Chip size="small" label={`${t.captureHttpStatus}: ${detail.http_status}`} />
              {detail.truncated && <Chip size="small" color="warning" label={t.captureTruncated} />}
              <Typography variant="body2" color="text.secondary">
                {t.captureCreatedAt}: {new Date(detail.created_at).toLocaleString()}
              </Typography>
            </Stack>

            <Accordion defaultExpanded disableGutters slotProps={{ transition: { timeout: 0 } }}>
              <AccordionSummary expandIcon={<ExpandMore />}>
                <Typography variant="subtitle2">{t.captureReqHeaders}</Typography>
              </AccordionSummary>
              <AccordionDetails>
                <HeaderTable t={t} headers={detail.req_headers} />
              </AccordionDetails>
            </Accordion>

            <Accordion defaultExpanded disableGutters slotProps={{ transition: { timeout: 0 } }}>
              <AccordionSummary expandIcon={<ExpandMore />}>
                <Typography variant="subtitle2">{t.captureReqBody}</Typography>
              </AccordionSummary>
              <AccordionDetails>
                <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mb: 1 }}>
                  <Button
                    size="small"
                    variant={reqView === 'pretty' ? 'contained' : 'outlined'}
                    onClick={() => setReqView('pretty')}
                  >
                    {t.capturePretty}
                  </Button>
                  <Button
                    size="small"
                    variant={reqView === 'raw' ? 'contained' : 'outlined'}
                    onClick={() => setReqView('raw')}
                  >
                    {t.captureRaw}
                  </Button>
                  <CopyButton t={t} text={reqBody} />
                </Stack>
                <Box component="pre" sx={preSx}>
                  {reqBody}
                </Box>
              </AccordionDetails>
            </Accordion>

            <Accordion defaultExpanded disableGutters slotProps={{ transition: { timeout: 0 } }}>
              <AccordionSummary expandIcon={<ExpandMore />}>
                <Typography variant="subtitle2">{t.captureRespHeaders}</Typography>
              </AccordionSummary>
              <AccordionDetails>
                <HeaderTable t={t} headers={detail.resp_headers} />
              </AccordionDetails>
            </Accordion>

            <Accordion defaultExpanded disableGutters slotProps={{ transition: { timeout: 0 } }}>
              <AccordionSummary expandIcon={<ExpandMore />}>
                <Typography variant="subtitle2">{t.captureRespBody}</Typography>
              </AccordionSummary>
              <AccordionDetails>
                <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mb: 1 }}>
                  <Button
                    size="small"
                    variant={respView === 'chat' ? 'contained' : 'outlined'}
                    onClick={() => setRespView('chat')}
                  >
                    {t.captureChat}
                  </Button>
                  <Button
                    size="small"
                    variant={respView === 'raw' ? 'contained' : 'outlined'}
                    onClick={() => setRespView('raw')}
                  >
                    {t.captureRaw}
                  </Button>
                  <CopyButton t={t} text={respBody} />
                </Stack>
                {respView === 'chat' ? (
                  <>
                    {respReasoning && (
                      <>
                        <Typography variant="overline" color="text.secondary">
                          {t.captureThinking}
                        </Typography>
                        <Box
                          component="pre"
                          sx={{ ...preSx, color: 'text.secondary', bgcolor: 'action.selected' }}
                        >
                          {respReasoning}
                        </Box>
                      </>
                    )}
                    <Typography variant="overline">{t.captureOutput}</Typography>
                    <Box component="pre" sx={preSx}>
                      {respOutput}
                    </Box>
                  </>
                ) : (
                  <Box component="pre" sx={preSx}>
                    {detail.resp_body}
                  </Box>
                )}
              </AccordionDetails>
            </Accordion>

            {(detail.translated_req_body ||
              detail.translated_resp_body ||
              detail.translated_req_headers ||
              detail.translated_resp_headers) && (
              <>
                <Typography variant="caption" color="text.secondary">
                  {t.captureTranslatedNote}
                </Typography>
                <Accordion disableGutters slotProps={{ transition: { timeout: 0 } }}>
                  <AccordionSummary expandIcon={<ExpandMore />}>
                    <Typography variant="subtitle2">{t.captureTranslatedReqTitle}</Typography>
                  </AccordionSummary>
                  <AccordionDetails>
                    {detail.translated_req_headers && (
                      <HeaderTable t={t} headers={detail.translated_req_headers} />
                    )}
                    {detail.translated_req_body && (
                      <>
                        <Stack direction="row" spacing={1} sx={{ alignItems: 'center', my: 1 }}>
                          <CopyButton t={t} text={detail.translated_req_body} />
                        </Stack>
                        <Box component="pre" sx={preSx}>
                          {detail.translated_req_body}
                        </Box>
                      </>
                    )}
                  </AccordionDetails>
                </Accordion>
                <Accordion disableGutters slotProps={{ transition: { timeout: 0 } }}>
                  <AccordionSummary expandIcon={<ExpandMore />}>
                    <Typography variant="subtitle2">{t.captureTranslatedRespTitle}</Typography>
                  </AccordionSummary>
                  <AccordionDetails>
                    {detail.translated_resp_headers && (
                      <HeaderTable t={t} headers={detail.translated_resp_headers} />
                    )}
                    {detail.translated_resp_body && (
                      <>
                        <Stack direction="row" spacing={1} sx={{ alignItems: 'center', my: 1 }}>
                          <CopyButton t={t} text={detail.translated_resp_body} />
                        </Stack>
                        <Box component="pre" sx={preSx}>
                          {detail.translated_resp_body}
                        </Box>
                      </>
                    )}
                  </AccordionDetails>
                </Accordion>
              </>
            )}

            <Typography variant="caption" color="text.secondary">
              {t.captureSecurityNote}
            </Typography>
          </Stack>
        )}
      </DialogContent>
      <DialogActions>
        {detail?.can_toggle_secret && (
          <Button onClick={() => onToggleSecret?.(!detail.secret)}>
            {detail.secret ? t.captureMarkVisible : t.captureMarkSecret}
          </Button>
        )}
        {detail && onRequestDelete && (
          <Button color="error" onClick={onRequestDelete}>
            {t.captureDelete}
          </Button>
        )}
        <Button onClick={onClose}>{t.captureClose}</Button>
      </DialogActions>
    </Dialog>
  );
}
