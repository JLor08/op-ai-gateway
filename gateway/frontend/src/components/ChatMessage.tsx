// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { memo, useEffect, useRef, useState } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import rehypeHighlight from 'rehype-highlight';
import remarkGfm from 'remark-gfm';
import { Box, Button, IconButton, Stack, TextField, Tooltip, Typography } from '@mui/material';
import ReplayIcon from '@mui/icons-material/Replay';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import CodeIcon from '@mui/icons-material/Code';
import EditIcon from '@mui/icons-material/Edit';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import type { ChatContent, ChatContentPart } from './shared/chatContent';
import { audioUrlsOf, hasRemovedAudio, imageUrlsOf, isMaskPart } from './shared/chatContent';
import { MASK_THUMBNAIL_BACKGROUND } from './shared/maskThumbnail';
import type { Translation } from './shared/types';
import { keyedUrls } from './shared/urlKeys';
import { ImageTurn } from './ImageTurn';
import { ImagePendingTurn } from './ImagePendingTurn';
import { AudioTurn } from './AudioTurn';
import { AudioPendingTurn } from './AudioPendingTurn';
import { AudioNotStoredTurn } from './AudioNotStoredTurn';
import { TranscriptionPendingTurn } from './TranscriptionPendingTurn';

function contentText(content: ChatContent): string {
  if (typeof content === 'string') return content;
  return content
    .filter((part): part is { type: 'text'; text: string } => part.type === 'text')
    .map((part) => part.text)
    .join('\n');
}

type ImagePart = Extract<ChatContentPart, { type: 'image_url' }>;

function imageParts(content: ChatContent): ImagePart[] {
  if (typeof content === 'string') return [];
  return content.filter((part): part is ImagePart => part.type === 'image_url');
}

// The MASK of a masked edit is an image part too, but it is not one of a message's
// images (imageUrlsOf) -- it has its own thumbnail and its own name.
function contentMask(content: ChatContent): string | undefined {
  return imageParts(content).find(isMaskPart)?.image_url.url;
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
  turnId,
  promptText,
  reasoning,
  reasoningMs,
  streaming,
  kind,
  elapsedMs,
  audioLive,
  audioMuted,
  onMuteAudio,
  onAbortAudio,
  ttftMs,
  tps,
  tokensPerSecond,
  onEdit,
  onRegenerate,
  onRefine,
  onDeleteAudio,
  canRun = true,
}: Readonly<{
  t: Translation;
  role: 'user' | 'assistant';
  content: ChatContent;
  // This row's transcript message id (`ChatUiMessage.id`), the same value the
  // Chat container already keys the row by. Passed down purely so a generated
  // image's DOWNLOAD NAME can be turn-unique — see ImageTurn's
  // downloadNameFor. Optional only because the standalone-props tests in this
  // component's own suite predate it; Chat.tsx, the single production call
  // site, always supplies it.
  turnId?: string;
  // The preceding user turn's text, used as a generated image's alt text (the
  // accessible name for "what was asked for" rather than a generic label).
  // Only meaningful on the assistant branch.
  promptText?: string;
  reasoning?: string;
  reasoningMs?: number;
  streaming?: boolean;
  // The kind of the RUN this row is streaming ("" text | "image" | "speech" | "transcription") --
  // ChatStore's runKind, read from the run's own reports (its 201, an
  // active-runs entry, an SSE snapshot), NOT the thread-level chatKind.
  // The distinction is the whole point: chatKind is the client's guess at
  // what the composer should offer and can be stale, while this is what the
  // executor is actually running, and rendering the image wait state over a
  // text run is exactly what the stale value produces. Only meaningful
  // together with `streaming`: it selects the image-run (or speech-run, or
  // transcription-run) pending render below for the one turn that is both streaming
  // and imageless (or audioless, or textless). Absent on every call site that
  // predates image threads.
  kind?: string;
  // The active run's server-anchored elapsed ms (ChatStore's runElapsedMs),
  // passed through untouched -- see ImagePendingTurn for how it is
  // interpolated into a ticking display.
  elapsedMs?: number;
  // Whether this streaming SPEECH run is playing its audio live right now (ChatStore's
  // runAudioLive, gated into the one streaming row like `kind`): the pending turn then
  // says so rather than promising that no news is coming. Only meaningful with
  // `streaming` and kind "speech".
  audioLive?: boolean;
  // The live speech run's controls (#281), threaded to the pending turn like `audioLive`
  // and, like it, supplied only to the one streaming row: whether the user has muted the
  // playback (ChatStore's runAudioMuted), the Mute toggle (called with the state it asks for)
  // and the Abort (the store's ordinary stop). The pending turn draws them only while live.
  audioMuted?: boolean;
  onMuteAudio?: (muted: boolean) => void;
  onAbortAudio?: () => void;
  ttftMs?: number;
  tps?: number;
  tokensPerSecond?: number;
  onEdit?: (text: string) => void;
  onRegenerate?: () => void;
  // Refine a generated image in place: given the url of ONE of this assistant turn's
  // images, the store makes it the source of the next edit. Supplied (by Chat.tsx, from
  // the store's per-message handler cache) only where that can run -- an image thread
  // whose model may be edited -- so a turn without it simply offers no control. Only
  // the assistant branch has images to refine; a user turn never shows one.
  onRefine?: (url: string) => void;
  // Delete the stored audio of THIS spoken answer (the store's per-message handler; the id is
  // already bound). The action stack offers it, behind an inline confirm, only on an answer that
  // holds audio -- never on a not-stored one, which has nothing left to delete. Only the
  // assistant branch has an answer's audio to delete; a user turn's audio is a transcription
  // clip and is never offered for it.
  onDeleteAudio?: () => void;
  // Whether a new run may be started. False while the model is unavailable —
  // the Edit/Regenerate controls (which each start a run) are disabled to match
  // the blocked Send. Defaults true so callers that don't gate stay unchanged.
  canRun?: boolean;
}>) {
  const text = contentText(content);
  const images = imageUrlsOf(content);
  const audioUrls = audioUrlsOf(content);
  // A speech turn's reply is one audio part when short and SEVERAL ordered parts when long;
  // the ASSISTANT turn renders them all as ONE player (AudioTurn stitches them into one
  // gapless clip), and the presence of any is what tells a finished speech turn from a
  // pending one. A USER turn's audio parts are a transcription turn's input clips and are
  // rendered one player per clip (the user branch below).
  const mask = contentMask(content);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(text);
  const [showRaw, setShowRaw] = useState(false);
  // The delete of the answer's audio asks first, in place: this is the question being open.
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const deleteButtonRef = useRef<HTMLButtonElement>(null);
  const restoreDeleteFocus = useRef(false);

  // Whether the audio of this answer can be deleted: it still holds some (a not-stored answer
  // has none, and its marker is not audio), its run is over (a streaming row's audio is still
  // arriving and the chat has a live run, which owns the transcript), and the row has a delete
  // to call. A question left open outlives none of that: it is dropped the moment the audio is
  // not deletable, so audio that comes back to this row later starts from the icon again.
  const canDeleteAudio =
    role === 'assistant' && !streaming && audioUrls.length > 0 && onDeleteAudio !== undefined;
  if (confirmingDelete && !canDeleteAudio) setConfirmingDelete(false);

  // The question takes the focus from the icon it replaces (its Cancel button is autofocused),
  // so cancelling hands it back to the icon. Accepting needs no such return: the audio and its
  // controls are about to go.
  useEffect(() => {
    if (!confirmingDelete && restoreDeleteFocus.current) {
      restoreDeleteFocus.current = false;
      deleteButtonRef.current?.focus();
    }
  }, [confirmingDelete]);

  const roleLabel = role === 'user' ? t.userRole : t.assistantRole;

  if (role === 'assistant') {
    // An image run emits ZERO incremental events -- the endpoint refuses
    // `stream` outright -- so there is nothing to count and the character
    // counter below would read "0 Zeichen" for the WHOLE multi-minute wait.
    // Three elements instead, each backed by a value that exists: liveness
    // from the run's own server-reported status (streaming), an elapsed
    // clock anchored on the server's own measurement, and one sentence
    // stating that no intermediate news is coming (see ImagePendingTurn).
    // `images.length === 0` keeps this from replacing an arriving image in
    // the same render, and this check runs BEFORE showReasoning below is
    // even computed, so the ordinary text-pending branch never sees it.
    if (streaming && kind === 'image' && images.length === 0) {
      return <ImagePendingTurn t={t} elapsedMs={elapsedMs ?? 0} roleLabel={roleLabel} />;
    }
    // A speech run is the same shape of wait -- one buffered WAV at the end, no
    // incremental events -- so it gets the same state, worded for audio, under the same
    // `no audio yet` guard that keeps it from replacing an arriving reply.
    if (streaming && kind === 'speech' && audioUrls.length === 0) {
      return (
        <AudioPendingTurn
          t={t}
          elapsedMs={elapsedMs ?? 0}
          roleLabel={roleLabel}
          live={audioLive === true}
          muted={audioMuted === true}
          onMute={onMuteAudio}
          onAbort={onAbortAudio}
        />
      );
    }
    // A transcription run is the same shape of wait until its FIRST text -- a one-shot
    // model answers one finished transcript at the end, a streaming one emits nothing
    // before its first delta -- but its answer is TEXT, so without this the ordinary
    // text-pending branch below would claim the turn: "Denkt ... 0 Zeichen" and a cursor
    // for a run that is not reasoning and emits nothing to count. `text.length === 0` is
    // the `no answer yet` guard that keeps it from replacing an arriving transcript (a
    // streaming model's first delta, a one-shot model's whole text), as images /
    // audio parts do for the kinds above.
    if (streaming && kind === 'transcription' && text.length === 0) {
      return <TranscriptionPendingTurn t={t} elapsedMs={elapsedMs ?? 0} roleLabel={roleLabel} />;
    }

    // A TERMINAL answer whose audio is gone (pruned, deleted, never stored or aborted) carries
    // the `audio_removed` marker in place of its audio parts, and shows the not-stored
    // placeholder where the player would be -- the answer's text, if it has any, stays above
    // it. Not while the run still streams (that row is the pending wait above, or a live
    // text answer), and not when audio is still stored (a player that plays beats a
    // placeholder, whatever else the content holds). A turn with neither audio nor marker is
    // a failed run and keeps rendering its error, not this.
    const audioRemoved = !streaming && audioUrls.length === 0 && hasRemovedAudio(content);

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
        {images.length > 0 && (
          <ImageTurn
            t={t}
            images={images}
            prompt={promptText}
            turnId={turnId ?? ''}
            onRefine={onRefine}
          />
        )}
        {audioUrls.length > 0 && <AudioTurn t={t} urls={audioUrls} turnId={turnId ?? ''} />}
        {audioRemoved && (
          <AudioNotStoredTurn t={t} onRegenerate={onRegenerate} disabled={!canRun} />
        )}
        <Stack direction="row" sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 0.5, mt: 1.25 }}>
          {/* The not-stored placeholder carries its own regenerate (the same handler), so the
              stack's would be a second, identically named button in one bubble. */}
          {onRegenerate && !audioRemoved && (
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
          {canDeleteAudio &&
            (confirmingDelete ? (
              // The confirm of the audio's delete: inline, in the place of the trash icon (not a
              // dialog over the conversation). It says the removal is final and that regenerate
              // is the way back. Cancel is the one that holds the focus: the safe answer.
              <Box
                role="group"
                aria-label={t.chatAudioDelete}
                sx={{ display: 'inline-flex', flexWrap: 'wrap', alignItems: 'center', gap: 0.5 }}
              >
                <Typography component="span" sx={{ color: 'var(--muted)', fontSize: 13, mx: 0.5 }}>
                  {t.chatAudioDeleteConfirm}
                </Typography>
                <Button
                  size="small"
                  color="error"
                  onClick={() => {
                    setConfirmingDelete(false);
                    onDeleteAudio?.();
                  }}
                >
                  {t.chatAudioDeleteAction}
                </Button>
                <Button
                  size="small"
                  color="secondary"
                  autoFocus
                  onClick={() => {
                    restoreDeleteFocus.current = true;
                    setConfirmingDelete(false);
                  }}
                >
                  {t.chatCancel}
                </Button>
              </Box>
            ) : (
              <Tooltip title={t.chatAudioDelete}>
                <IconButton
                  ref={deleteButtonRef}
                  size="small"
                  aria-label={t.chatAudioDelete}
                  onClick={() => setConfirmingDelete(true)}
                >
                  <DeleteOutlineIcon fontSize="small" />
                </IconButton>
              </Tooltip>
            ))}
          {(ttftMs !== undefined || tps !== undefined || tokensPerSecond !== undefined) && (
            <Typography
              component="span"
              sx={{ ml: 1, color: 'var(--muted)', fontSize: 12, whiteSpace: 'nowrap' }}
            >
              {[
                ttftMs !== undefined ? `${t.chatTtftLabel} ${(ttftMs / 1000).toFixed(1)}s` : '',
                tps !== undefined ? `${Math.round(tps)} ${t.chatCharsPerSecUnit}` : '',
                // tokens/sec appears next to chars/s once the turn completes and
                // the upstream reported usage (issue #56).
                tokensPerSecond !== undefined
                  ? `${Math.round(tokensPerSecond)} ${t.chatTokensPerSecUnit}`
                  : '',
              ]
                .filter(Boolean)
                .join(' · ')}
            </Typography>
          )}
        </Stack>
      </Box>
    );
  }

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
          {(images.length > 0 || mask !== undefined) && (
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
              {mask !== undefined && (
                <Box
                  component="img"
                  src={mask}
                  alt={t.chatAttachedMask}
                  sx={{
                    display: 'block',
                    width: 72,
                    height: 72,
                    objectFit: 'cover',
                    border: '1px solid var(--line)',
                    borderRadius: '8px',
                    background: MASK_THUMBNAIL_BACKGROUND,
                  }}
                />
              )}
            </Box>
          )}
          {/* A transcription turn's input clip: the user's own audio, played from the same
              fetch-once object URL (revoked on unmount) as a speech reply, and offered for
              download. */}
          {keyedUrls(audioUrls).map(({ key, url }) => (
            <AudioTurn key={key} t={t} urls={[url]} turnId={turnId ?? ''} variant="input" />
          ))}
          {/* No edit on a turn that carries audio: it is a transcription turn, whose clip is
              all the run reads -- editing its (empty) text would change nothing a run uses
              and a replay would transcribe the same clip. */}
          {onEdit && audioUrls.length === 0 && (
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
