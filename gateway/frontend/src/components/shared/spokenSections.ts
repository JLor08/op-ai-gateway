// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The text side of a voice answer's spoken sections (#315): which parts of the full clean answer
// were spoken, and the body left around them. The gateway persists the answer once, whole, and
// the sections as spans into it (`spoken_sections`); the transcript shows each span as its own
// collapsed block AT ITS POSITION and the rest as ordinary body text.

// One spoken section as the gateway persists it: a RUNE (code point) offset and length into the
// answer's text, plus the spoken text itself, kept beside the span for convenience. The span is
// authoritative; `text` is optional and only a cross-check.
export type SpokenSection = { text?: string; start: number; len: number };

// The answer laid out for the transcript: body text and spoken sections, in document order. A
// section carries the index it has in `spoken_sections` -- the key to its audio -- even when a
// neighbour could not be placed and is missing here.
export type AnswerSegment =
  { kind: 'body'; text: string } | { kind: 'section'; text: string; index: number };

// The text without its leading blank space: whole blank lines go, but the indentation of the
// first line with content stays (it can be a markdown code block).
export function dropLeadingBlank(text: string): string {
  const lead = text.length - text.trimStart().length;
  if (lead === 0) return text;
  const lineBreak = text.lastIndexOf('\n', lead - 1);
  return text.slice(lineBreak >= 0 ? lineBreak + 1 : lead);
}

// Where a section sits, as a [start, end) range of code points, or null when it cannot be found.
type Placement = { start: number; end: number };

function placeSection(
  chars: readonly string[],
  section: SpokenSection,
  floor: number,
): Placement | null {
  const want = section.text?.trim() ?? '';
  const end = section.start + section.len;
  // The persisted span, when it lies after the previous section and (given a text) holds it.
  if (
    Number.isInteger(section.start) &&
    Number.isInteger(section.len) &&
    section.len > 0 &&
    section.start >= floor &&
    end <= chars.length &&
    (want === '' || chars.slice(section.start, end).join('') === want)
  ) {
    return { start: section.start, end };
  }
  if (want === '') return null;
  // A span that does not line up: find the text by value, after the previous section. The
  // gateway stores a verbatim slice of the answer, so it is found as one.
  const rest = chars.slice(floor).join('');
  const at = rest.indexOf(want);
  if (at < 0) return null;
  const start = floor + Array.from(rest.slice(0, at)).length;
  return { start, end: start + Array.from(want).length };
}

// The gap before a section, ready to show: a lead-in loses its trailing space and, between two
// sections, the gap its leading blank lines too. Blank means there is nothing to show.
function bodyBetween(chars: readonly string[], from: number, to: number, first: boolean): string {
  const gap = chars.slice(from, to).join('');
  return first ? gap.trimEnd() : dropLeadingBlank(gap).trimEnd();
}

// segmentAnswer lays the full clean answer out as [body][section][body][section]...[body]: each
// section is cut out of the text at its span and the text around it is the body, so every word
// is shown exactly once. Body that is only whitespace is left out.
//
// Spans are RUNE offsets (Array.from, not UTF-16 units), so a multi-byte character (an umlaut, an
// emoji) before a section does not shift the cut. A span that does not line up (out of range,
// reaching back over the previous section, or not holding the section's text) falls back to
// finding the text by value after the previous section. A section that still cannot be placed
// is shown with its text and cuts nothing -- showing a spoken text twice beats dropping part of
// the answer -- and one with no text to show either is left out.
export function segmentAnswer(text: string, sections: readonly SpokenSection[]): AnswerSegment[] {
  const chars = Array.from(text);
  const segments: AnswerSegment[] = [];
  let cursor = 0;
  let first = true;
  sections.forEach((section, index) => {
    const placed = placeSection(chars, section, cursor);
    if (placed === null) {
      const shown = section.text?.trim() ?? '';
      if (shown !== '') segments.push({ kind: 'section', text: shown, index });
      return;
    }
    const before = bodyBetween(chars, cursor, placed.start, first);
    if (before.trim() !== '') segments.push({ kind: 'body', text: before });
    segments.push({
      kind: 'section',
      text: chars.slice(placed.start, placed.end).join('').trim(),
      index,
    });
    cursor = placed.end;
    first = false;
  });
  const tail = dropLeadingBlank(chars.slice(cursor).join(''));
  if (tail.trim() !== '') segments.push({ kind: 'body', text: tail });
  return segments;
}
