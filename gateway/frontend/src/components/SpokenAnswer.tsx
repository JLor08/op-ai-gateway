// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Box } from '@mui/material';
import type { AnswerSegment } from './shared/spokenSections';
import type { Translation } from './shared/types';
import { AnswerBody } from './AnswerBody';
import { AudioNotStoredTurn } from './AudioNotStoredTurn';
import { AudioTurn } from './AudioTurn';

// One spoken block of a voice answer (#314): a collapsed <details> whose summary line carries the
// label and the audio node (the player, or the not-stored placeholder that stands in for it) and
// whose body is the text that was spoken. `number` tells several blocks apart.
export function SpokenBlock({
  t,
  number,
  open,
  text,
  audio,
}: Readonly<{
  t: Translation;
  number?: number;
  open: boolean;
  text: string;
  audio: ReactNode;
}>) {
  return (
    <Box
      component="details"
      open={open}
      sx={{
        mt: 1.25,
        mb: 1.25,
        border: '1px solid var(--line)',
        bgcolor: 'var(--page)',
        borderRadius: '8px',
        fontSize: 13,
        '& > summary': {
          cursor: 'pointer',
          p: '8px 12px',
          listStyle: 'none',
          display: 'flex',
          flexWrap: 'wrap',
          alignItems: 'center',
          gap: 1,
          color: 'var(--muted)',
          fontWeight: 700,
          '&::-webkit-details-marker': { display: 'none' },
          // The marker a flex summary loses: a caret that says whether it is open.
          '&::before': { content: String.raw`"\25B8"` },
        },
        '&[open] > summary::before': { content: String.raw`"\25BE"` },
        // The player (or the not-stored line) rides the summary line; its own bottom
        // margin is for the stack it normally sits in.
        '& > summary > div': { mb: 0 },
      }}
    >
      <summary>
        <span>{t.chatVoiceSpokenSummary}</span>
        {number !== undefined && <span>{` ${number}`}</span>}
        {audio}
      </summary>
      <Box
        sx={{
          p: '10px 12px',
          color: 'var(--text)',
          lineHeight: 1.5,
          whiteSpace: 'pre-wrap',
          overflowWrap: 'anywhere',
        }}
      >
        {text}
      </Box>
    </Box>
  );
}

// Whether a voice answer's stored audio can be handed to its sections by ORDER: one clip per
// section. The committed content is [text, section 0's audio, section 1's, ...], and a section's
// audio is one part -- or several when it is long enough to be cut into parts of a bounded size --
// with nothing in the parts to say where one section ends and the next begins. So equal counts are
// the only proof of one clip per section, and a mismatch is not guessed at (see SpokenAnswer).
export function audioPairsBySection(audioCount: number, sectionCount: number): boolean {
  return audioCount === sectionCount;
}

// A voice answer with spoken sections (#315), laid out in document order: the body text around
// the sections as plain answer text, and each section as its own collapsed, replayable block AT
// ITS POSITION. `segments` come from segmentAnswer, so every word of the answer shows exactly once.
//
// Each block replays its OWN section's audio, a player over that section's clip (AudioTurn
// stitches a clip's parts, and fetches it once). When the audio is gone (the answer carries the
// removed marker: pruned, deleted, never stored) every block shows the not-stored placeholder
// instead and only the first offers the regenerate: it regenerates the whole answer, and one
// button says so. Where the caller can speak a section again (`onResynthSection`, #324) EVERY
// placeholder also carries a play control for its OWN section, which synthesizes that section's
// text on demand without touching the answer. Audio that is not there and not marked removed
// leaves the blocks text-only.
// Audio that cannot be paired with the sections is not played here at all; the caller then puts
// the answer's one stitched player below (audioPairsBySection).
//
// The blocks open by themselves only when they are all there is to read -- the phase-2 rule
// for one block, applied to the answer without ANY section -- and are collapsed while there is
// body beside them, which they would only repeat.
export function SpokenAnswer({
  t,
  segments,
  sectionCount,
  audioUrls,
  audioRemoved,
  turnId,
  raw,
  onRegenerate,
  onResynthSection,
  canRun,
}: Readonly<{
  t: Translation;
  segments: readonly AnswerSegment[];
  // How many sections the turn recorded: the count the audio is paired against.
  sectionCount: number;
  audioUrls: readonly string[];
  audioRemoved: boolean;
  turnId: string;
  raw: boolean;
  onRegenerate?: () => void;
  // Speaks one section's text again with the thread's current speech model and voice and hands
  // back the audio (ChatStore, #324). Absent when no speech model resolves; the placeholders then
  // offer the whole-answer regenerate alone.
  onResynthSection?: (sectionText: string) => Promise<Blob>;
  canRun: boolean;
}>) {
  const hasBody = segments.some((segment) => segment.kind === 'body');
  const blockCount = segments.filter((segment) => segment.kind === 'section').length;
  const firstBlock = segments.find((segment) => segment.kind === 'section');
  const paired = audioUrls.length > 0 && audioPairsBySection(audioUrls.length, sectionCount);

  const audioOf = (index: number, text: string, first: boolean): ReactNode => {
    if (paired) {
      // A download is named after the turn; with several blocks the section is part of the
      // name too, or the second save would silently become "(1)".
      const id = sectionCount > 1 ? [turnId, `s${index + 1}`].filter(Boolean).join('-') : turnId;
      return <AudioTurn t={t} urls={[audioUrls[index]]} turnId={id} />;
    }
    if (audioUrls.length === 0 && audioRemoved) {
      return (
        <AudioNotStoredTurn
          t={t}
          onRegenerate={first ? onRegenerate : undefined}
          onResynth={onResynthSection ? () => onResynthSection(text) : undefined}
          disabled={!canRun}
        />
      );
    }
    return null;
  };

  // Keys follow what a piece is NEXT to, not its place in the list: a section is keyed by its own
  // index and a body by the section it follows, so a piece keeps its identity (and its audio
  // player its loaded clip) however the pieces around it come and go.
  let follows = -1;
  let shown = 0;
  return (
    <>
      {segments.map((segment) => {
        if (segment.kind === 'body') {
          return <AnswerBody key={`body-after-${follows}`} text={segment.text} raw={raw} />;
        }
        follows = segment.index;
        shown += 1;
        return (
          <SpokenBlock
            key={`section-${segment.index}`}
            t={t}
            number={blockCount > 1 ? shown : undefined}
            open={!hasBody}
            text={segment.text}
            audio={audioOf(segment.index, segment.text, segment === firstBlock)}
          />
        );
      })}
    </>
  );
}
