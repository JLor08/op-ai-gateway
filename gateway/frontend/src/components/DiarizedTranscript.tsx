// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box } from '@mui/material';
import type { SpeakerTranscriptPart, SpeakerTurn } from './shared/chatContent';
import type { Translation } from './shared/types';

// The stripe colours of the speakers, taken in turn. They only DECORATE: the label says who is
// speaking, so the view is as readable without telling the colours apart as with it. Mid-tone
// hues, so a stripe holds on the light and the dark surface alike; a transcript with more
// speakers than colours goes round again (the label still tells them apart).
const SPEAKER_STRIPES = [
  '#0d9488',
  '#6366f1',
  '#d97706',
  '#db2777',
  '#0284c7',
  '#7c3aed',
  '#65a30d',
  '#dc2626',
] as const;

// The number each distinct speaker is shown under: 1, 2, ... in the order the speakers first
// spoke. The ids the model reports are free-form strings (an integer today, but "SPEAKER_01",
// a name or the empty string are all valid ids), so nothing is assumed of them beyond "same
// string, same speaker" -- which also makes a blank id a speaker like any other, with a label
// of its own, and a count of speakers unbounded.
function speakerNumbers(turns: readonly SpeakerTurn[]): Map<string, number> {
  const numbers = new Map<string, number>();
  for (const turn of turns) {
    if (!numbers.has(turn.speaker)) numbers.set(turn.speaker, numbers.size + 1);
  }
  return numbers;
}

// A point in the clip as a clock: m:ss, or h:mm:ss from an hour on. Whole seconds -- the turns
// are speech, not frames -- rounded, and never negative.
function clockTime(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = String(total % 60).padStart(2, '0');
  if (hours === 0) return `${minutes}:${rest}`;
  return `${hours}:${String(minutes).padStart(2, '0')}:${rest}`;
}

// The span a turn covers, "0:03–0:06", or undefined when the turn has no start (the times are
// omitted when unknown, and a span that is only an end would read as a start). A start alone
// is shown as it is.
function timeSpan(turn: SpeakerTurn): string | undefined {
  if (turn.start === undefined) return undefined;
  const start = clockTime(turn.start);
  return turn.end === undefined ? start : `${start}–${clockTime(turn.end)}`;
}

// The turns paired with the key each is rendered under. A turn has no id of its own, so its key
// is its content: speaker, span and text. Turns that repeat in all of them (a "Mhm." said twice by
// one speaker, no times given) are told apart by how many of the same came before, so the keys are
// unique without being the turn's position.
function keyedTurns(turns: readonly SpeakerTurn[]): { key: string; turn: SpeakerTurn }[] {
  const seen = new Map<string, number>();
  return turns.map((turn) => {
    const content = JSON.stringify([turn.speaker, turn.start, turn.end, turn.text]);
    const occurrence = (seen.get(content) ?? 0) + 1;
    seen.set(content, occurrence);
    return { key: occurrence === 1 ? content : `${content}#${occurrence}`, turn };
  });
}

// The speaker view as plain text, what the Copy control hands over: one line per turn behind its
// speaker's label, in the order spoken. A transcript with no turns to show is just its text.
export function diarizedTranscriptText(t: Translation, part: SpeakerTranscriptPart): string {
  if (part.turns.length === 0) return part.text;
  const numbers = speakerNumbers(part.turns);
  return part.turns
    .map((turn) => `${t.chatTranscriptionSpeaker(numbers.get(turn.speaker) ?? 1)}: ${turn.text}`)
    .join('\n');
}

const textSx = {
  color: 'var(--text)',
  lineHeight: 1.5,
  whiteSpace: 'pre-wrap',
  overflowWrap: 'anywhere',
} as const;

// A diarized transcription turn (#295), laid out as who said what: one block per turn, in the
// order the turns were spoken, each with its speaker's label, the time it covers (when the model
// said) and its text -- the speaker-block layout of a voice answer's SpokenAnswer, minus the
// audio, which a transcription turn has on the USER side (the clip it transcribes).
//
// The turn's body is this and nothing else: a diarized turn commits the speaker_transcript part
// and no text part beside it, so there is no text for the answer body to show. `raw` is the
// view-raw toggle's state: the whole transcript as one plain text, without the speakers. A part
// with no turns left to show (the history of a turn that was cut down, see speakerTranscriptOf)
// shows that same plain text, so the transcript is never lost to the layout.
export function DiarizedTranscript({
  t,
  part,
  raw = false,
}: Readonly<{
  t: Translation;
  part: SpeakerTranscriptPart;
  raw?: boolean;
}>) {
  if (raw || part.turns.length === 0) {
    return <Box sx={{ mt: 1.25, ...textSx }}>{part.text}</Box>;
  }
  const numbers = speakerNumbers(part.turns);
  return (
    <Box
      component="ol"
      // listStyle: 'none' drops the list semantics in Safari/VoiceOver, so the role puts them back.
      role="list"
      aria-label={t.chatTranscriptionSpeakerView}
      sx={{ listStyle: 'none', m: 0, mt: 1.25, p: 0, display: 'grid', gap: 1 }}
    >
      {keyedTurns(part.turns).map(({ key, turn }) => {
        const number = numbers.get(turn.speaker) ?? 1;
        const span = timeSpan(turn);
        return (
          <Box
            component="li"
            key={key}
            sx={{
              p: '8px 12px',
              border: '1px solid var(--line)',
              borderLeft: `4px solid ${SPEAKER_STRIPES[(number - 1) % SPEAKER_STRIPES.length]}`,
              bgcolor: 'var(--page)',
              borderRadius: '8px',
              fontSize: 13,
            }}
          >
            <Box
              sx={{
                display: 'flex',
                flexWrap: 'wrap',
                alignItems: 'baseline',
                gap: 1,
                color: 'var(--muted)',
                fontWeight: 700,
              }}
            >
              <span>{t.chatTranscriptionSpeaker(number)}</span>
              {span !== undefined && (
                <Box component="span" sx={{ fontWeight: 400 }}>
                  {span}
                </Box>
              )}
            </Box>
            <Box sx={{ mt: 0.5, fontSize: 14, ...textSx }}>{turn.text}</Box>
          </Box>
        );
      })}
    </Box>
  );
}
