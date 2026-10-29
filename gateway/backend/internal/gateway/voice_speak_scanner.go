// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"strings"
	"unicode/utf8"
)

// maxSpeakTagLen bounds how many bytes the scanner will hold back while it decides
// whether a '<' starts a <speak ...> / </speak> tag. A real tag is far shorter; a tag
// that is not complete within this many bytes is treated as ordinary text, so a stray
// "<speak " in the model output can never stall the published stream.
const maxSpeakTagLen = 512

// speakRegion is one captured <speak lang="..."> ... </speak> region.
type speakRegion struct {
	// Text is the region's content, tags removed, exactly as streamed (not trimmed).
	Text string
	// Lang is the open tag's lang attribute, verbatim; "" when it had none.
	Lang string
	// RuneOffset is the position of the region's content START within the clean text the
	// scanner emits, counted in runes (code points), not bytes, so the portal can slice the
	// region out of the full answer by position in a way the frontend can reproduce on a JS
	// string. It points at the first rune of Text even when the caller later trims Text; the
	// caller advances it past any whitespace it strips.
	RuneOffset int
}

// speakScanner extracts every <speak lang="..."> ... </speak> region, in order, from an LLM
// token stream (the Voice Chat spoken sections) while stripping every speak tag from the
// text that is published to the transcript.
//
// The stream is fed delta by delta through Write. Text outside a tag is published
// immediately; only the bytes of a possible tag that is still being parsed are held back
// until the next delta resolves them (or Close flushes them as plain text). The regions'
// text and the text between them all pass through the clean stream -- the tags are the only
// thing removed. A <speak> tag nested inside an open region and a self-closing <speak/> tag
// are stripped but open no region.
//
// The scanner works on bytes. '<' is ASCII and never part of a multi-byte UTF-8 sequence,
// so a rune split across two deltas is passed through untouched like any other text.
// It is not safe for concurrent use; one scanner serves one stream.
type speakScanner struct {
	// pending holds the bytes of a possible tag (always starting with '<') that the last
	// delta ended in the middle of.
	pending string
	// found is set once any <speak> tag has been seen, a self-closing one included.
	found bool
	// selfClosingLang is the lang of the first self-closing tag seen while no region had opened;
	// Summary reports it when the stream ends up with no region at all.
	selfClosingLang string
	// regions holds every region opened so far, in order. While open is set the last entry is
	// the region still being read: its Text is not filled in yet (it accumulates in text) and
	// Regions leaves it out.
	regions []speakRegion
	// open is true while the stream is between a region's open tag and its close tag.
	open bool
	// text accumulates the open region's content; it moves into regions on close.
	text strings.Builder
	// runes counts the runes of the clean text emitted so far -- the offset the next region's
	// content starts at. A multi-byte rune can be split across two deltas, so the bytes of an
	// incomplete trailing sequence wait in runeCarry and are counted once, over the reassembled
	// whole, when the rest arrives.
	runes     int
	runeCarry string
}

// newSpeakScanner returns a scanner ready for the start of a stream.
func newSpeakScanner() *speakScanner {
	return &speakScanner{}
}

// Write feeds one raw content delta. It returns the tag-stripped text that is safe to
// publish now, and whether a <speak> region closed on this delta.
//
// When closed is true, closedIdx is the index (into Regions) of the region that closed. A
// delta that completes more than one region -- a whole answer arriving in one chunk -- reports
// the LAST of them: every region up to and including closedIdx is final in Regions, so a
// caller that remembers the next index it has not handled never skips one. closedIdx is
// meaningless when closed is false.
func (s *speakScanner) Write(delta string) (clean string, closedIdx int, closed bool) {
	in := s.pending + delta
	s.pending = ""

	var out strings.Builder
	for in != "" {
		lt := strings.IndexByte(in, '<')
		if lt < 0 {
			s.emit(&out, in)
			break
		}
		s.emit(&out, in[:lt])
		in = in[lt:]

		kind, n, attrs := matchSpeakTag(in)
		switch kind {
		case speakTagPartial:
			// The delta ended inside a possible tag: wait for the next one.
			s.pending = in
			return out.String(), closedIdx, closed
		case speakTagOpen:
			s.openTag(attrs)
			in = in[n:]
		case speakTagSelfClosing:
			s.selfClosingTag(attrs)
			in = in[n:]
		case speakTagClose:
			if idx, ok := s.closeTag(); ok {
				closedIdx, closed = idx, true
			}
			in = in[n:]
		default:
			// Not a speak tag: the '<' is ordinary text. Rescan from the next byte so a
			// '<' later in the same run is still considered.
			s.emit(&out, "<")
			in = in[1:]
		}
	}
	return out.String(), closedIdx, closed
}

// Close flushes the scanner at the end of the stream. Bytes held back for a tag that
// never completed are emitted as plain text. The one exception is the open region: a
// half-written </speak> at the end of a still-open region is markup, not speech, so those
// bytes reach the clean stream but are kept out of the captured region. A region still open
// at the end is finished here, so Regions is complete after Close and includes it with the
// text streamed so far.
func (s *speakScanner) Close() (clean string) {
	pending := s.pending
	s.pending = ""
	var out strings.Builder
	if s.open && isPartialSpeakClose(pending) {
		out.WriteString(pending)
	} else {
		s.emit(&out, pending)
	}
	s.finishRegion()
	return out.String()
}

// Regions returns the captured regions in stream order. A region is listed once its </speak>
// has been seen: the one still being read is left out until it closes, so every entry is
// final. After Close the list is complete, and a region left unclosed by the end of the
// stream is its last entry. The slice must not be modified by the caller.
func (s *speakScanner) Regions() []speakRegion {
	n := len(s.regions)
	if s.open {
		n--
	}
	// The capacity cap keeps a caller's append from writing into the scanner's own backing array.
	return s.regions[:n:n]
}

// Summary returns the FIRST region's captured text and lang -- the phase-2 single-summary
// view of the stream. A region still being read yields the text captured so far. With no
// region it returns empty text, and the lang of a self-closing tag if one was seen; with no
// <speak> tag at all both are empty.
func (s *speakScanner) Summary() (text string, lang string) {
	if len(s.regions) == 0 {
		return "", s.selfClosingLang
	}
	first := s.regions[0]
	if s.open && len(s.regions) == 1 {
		return s.text.String(), first.Lang
	}
	return first.Text, first.Lang
}

// Found reports whether any <speak> tag was seen, a self-closing one included.
func (s *speakScanner) Found() bool {
	return s.found
}

// SummaryStart returns the rune offset of the first <speak> region's content within the clean
// text the scanner emits (the first region's RuneOffset), or -1 when no region was captured (no
// tag, or only self-closing tags that open none). It is a count of code points, not bytes. The
// offset is to the region's content START even when the content is later trimmed; the caller
// advances it past any whitespace it strips.
func (s *speakScanner) SummaryStart() int {
	if len(s.regions) == 0 {
		return -1
	}
	return s.regions[0].RuneOffset
}

// emit publishes text to the clean stream, to the open region while inside one, and into the
// running rune count that stamps each region's offset.
func (s *speakScanner) emit(out *strings.Builder, text string) {
	if text == "" {
		return
	}
	out.WriteString(text)
	s.countRunes(text)
	if s.open {
		s.text.WriteString(text)
	}
}

// countRunes adds the runes of text, a piece of the clean stream, to the running count. A
// trailing byte sequence that is a valid but incomplete rune is held in runeCarry and counted
// with the next piece, so a rune cut across two deltas counts once however the stream is chunked.
func (s *speakScanner) countRunes(text string) {
	text = s.runeCarry + text
	cut := len(text)
	// An incomplete rune is at most utf8.UTFMax-1 bytes: a lead byte plus continuation bytes.
	for i := 1; i < utf8.UTFMax && i <= len(text); i++ {
		if utf8.RuneStart(text[len(text)-i]) {
			if !utf8.FullRuneInString(text[len(text)-i:]) {
				cut = len(text) - i
			}
			break
		}
	}
	s.runes += utf8.RuneCountInString(text[:cut])
	s.runeCarry = text[cut:]
}

// openTag handles a <speak ...> tag. Outside a region it opens the next one, stamping the
// clean-text offset its content begins at. A tag inside an already open region is merely
// stripped: the region ends at the first close tag, and its lang stays the outer tag's.
func (s *speakScanner) openTag(attrs string) {
	s.found = true
	if s.open {
		return
	}
	s.open = true
	s.regions = append(s.regions, speakRegion{
		Lang: parseSpeakLang(attrs),
		// A dangling incomplete rune counts as one rune here, as it would if the stream ended.
		RuneOffset: s.runes + utf8.RuneCountInString(s.runeCarry),
	})
}

// selfClosingTag handles a <speak .../> tag. It is stripped and counts as a speak tag that
// was seen (Found), but it opens no region: there is no text to capture and the rest of the
// stream stays ordinary text. Its lang is kept only as the fallback Summary reports when no
// region ever opens; the first one seen wins, and one inside or after a region changes nothing.
func (s *speakScanner) selfClosingTag(attrs string) {
	if !s.found {
		s.selfClosingLang = parseSpeakLang(attrs)
	}
	s.found = true
}

// closeTag handles a </speak> tag. It finishes the open region and returns its index; a
// stray close tag with no region open does nothing and reports false.
func (s *speakScanner) closeTag() (idx int, closed bool) {
	if !s.open {
		return 0, false
	}
	s.finishRegion()
	return len(s.regions) - 1, true
}

// finishRegion moves the open region's accumulated text into its entry and ends it. It is a
// no-op when no region is open.
func (s *speakScanner) finishRegion() {
	if !s.open {
		return
	}
	s.regions[len(s.regions)-1].Text = s.text.String()
	s.text.Reset()
	s.open = false
}

type speakTagKind int

const (
	// speakTagNone: the bytes are definitely not a speak tag.
	speakTagNone speakTagKind = iota
	// speakTagPartial: the bytes are a prefix of a possible speak tag; more are needed.
	speakTagPartial
	// speakTagOpen: a complete <speak ...> tag.
	speakTagOpen
	// speakTagClose: a complete </speak> tag.
	speakTagClose
	// speakTagSelfClosing: a complete <speak .../> tag, whose '>' directly follows a '/'.
	speakTagSelfClosing
)

// matchSpeakTag classifies the text at the start of b, which must begin with '<'. For a
// complete tag n is its length in bytes and, for an open or self-closing tag, attrs is the
// raw attribute text between the tag name and the closing '>' (without the self-closing
// '/'). Tag names match case-insensitively.
//
// Only the first maxSpeakTagLen bytes are ever examined, so the verdict for a given tag
// does not depend on how the stream was cut into deltas.
func matchSpeakTag(b string) (kind speakTagKind, n int, attrs string) {
	capped := len(b) > maxSpeakTagLen
	if capped {
		b = b[:maxSpeakTagLen]
	}
	// A prefix that is still undecided when the cap is reached will never resolve into a
	// tag, so it is plain text.
	partial := speakTagPartial
	if capped {
		partial = speakTagNone
	}

	pos, closing, verdict, ok := scanSpeakName(b, partial)
	if !ok {
		return verdict, 0, ""
	}
	if closing {
		kind, n = scanSpeakClose(b, pos, partial)
		return kind, n, ""
	}
	return scanSpeakOpen(b, pos, partial)
}

// scanSpeakName matches a leading '<' or '</' followed by the tag name "speak"
// (case-insensitive). On success it returns the index just past the name, whether the tag is
// a closing one, and ok=true. When the bytes are not "<speak"/"</speak" it returns ok=false
// with the verdict to report: partial (a prefix that may still complete) or speakTagNone (a
// different name such as <speaker>, decided already); closing is still reported either way.
func scanSpeakName(b string, partial speakTagKind) (pos int, closing bool, verdict speakTagKind, ok bool) {
	pos = 1
	closing = len(b) > 1 && b[1] == '/'
	if closing {
		pos = 2
	}
	const name = "speak"
	for i := range len(name) {
		if pos+i >= len(b) {
			return 0, closing, partial, false
		}
		if asciiLower(b[pos+i]) != name[i] {
			return 0, closing, speakTagNone, false
		}
	}
	pos += len(name)
	if pos >= len(b) {
		return 0, closing, partial, false
	}
	return pos, closing, speakTagNone, true
}

// scanSpeakClose matches the tail of a </speak ...> tag from pos (just past the name):
// optional whitespace before the '>'. partial is the verdict for bytes that run out before
// resolving.
func scanSpeakClose(b string, pos int, partial speakTagKind) (kind speakTagKind, n int) {
	for ; pos < len(b); pos++ {
		switch {
		case b[pos] == '>':
			return speakTagClose, pos + 1
		case !isSpeakSpace(b[pos]):
			return speakTagNone, 0
		}
	}
	return partial, 0
}

// scanSpeakOpen matches the tail of a <speak ...> tag from pos (just past the name): '>'
// immediately, '/>' (self-closing), or whitespace then attributes up to '>'. Anything else
// after the name is a longer tag name such as <speaker>. A tag whose '>' is directly preceded
// by '/' is self-closing and reported as speakTagSelfClosing. attrs is the raw attribute text
// between the name and the closing '>' (minus the self-closing '/'); partial is the verdict
// for bytes that run out before resolving.
func scanSpeakOpen(b string, pos int, partial speakTagKind) (kind speakTagKind, n int, attrs string) {
	switch {
	case b[pos] == '>':
		return speakTagOpen, pos + 1, ""
	case b[pos] == '/':
		if pos+1 >= len(b) {
			return partial, 0, ""
		}
		if b[pos+1] == '>' {
			return speakTagSelfClosing, pos + 2, ""
		}
		return speakTagNone, 0, ""
	case !isSpeakSpace(b[pos]):
		return speakTagNone, 0, ""
	}
	attrStart := pos
	var quote byte
	for ; pos < len(b); pos++ {
		c := b[pos]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			if b[pos-1] == '/' {
				return speakTagSelfClosing, pos + 1, b[attrStart : pos-1]
			}
			return speakTagOpen, pos + 1, b[attrStart:pos]
		}
	}
	return partial, 0, ""
}

// isPartialSpeakClose reports whether b is the unfinished start of a </speak> tag: "</"
// followed by a prefix of the name, or the whole name and then whitespace, with the '>' still
// missing. A lone '<' is not enough, since it may equally begin an open tag.
func isPartialSpeakClose(b string) bool {
	if len(b) < 2 || b[1] != '/' {
		return false
	}
	kind, _, _ := matchSpeakTag(b)
	return kind == speakTagPartial
}

// parseSpeakLang returns the value of the lang attribute in the raw attribute text of an
// open tag, verbatim, or "" when there is none. It is tolerant: the attribute name is
// case-insensitive, the value may be double-quoted, single-quoted or bare, whitespace is
// allowed around '=', and any other attribute is skipped.
func parseSpeakLang(attrs string) string {
	for i := 0; i < len(attrs); {
		i = skipSpeakSpace(attrs, i)
		name, next := readSpeakAttrName(attrs, i)
		i = skipSpeakSpace(attrs, next)
		if i >= len(attrs) || attrs[i] != '=' {
			// A valueless attribute (or trailing junk): nothing to read, move on.
			continue
		}
		i = skipSpeakSpace(attrs, i+1) // past '='
		value, next := readSpeakAttrValue(attrs, i)
		i = next
		if strings.EqualFold(name, "lang") {
			return value
		}
	}
	return ""
}

// skipSpeakSpace returns the index of the first non-whitespace byte at or after i.
func skipSpeakSpace(attrs string, i int) int {
	for i < len(attrs) && isSpeakSpace(attrs[i]) {
		i++
	}
	return i
}

// readSpeakAttrName reads an attribute name starting at i (already past whitespace): the run
// up to the next whitespace or '='. It returns the name and the index just past it.
func readSpeakAttrName(attrs string, i int) (name string, next int) {
	start := i
	for i < len(attrs) && !isSpeakSpace(attrs[i]) && attrs[i] != '=' {
		i++
	}
	return attrs[start:i], i
}

// readSpeakAttrValue reads an attribute value starting at i (already past '=' and any
// whitespace): a double- or single-quoted string (an unterminated quote runs to the end), or
// a bare run up to the next whitespace. It returns the value and the index just past it.
func readSpeakAttrValue(attrs string, i int) (value string, next int) {
	if i < len(attrs) && (attrs[i] == '"' || attrs[i] == '\'') {
		quote := attrs[i]
		i++
		end := strings.IndexByte(attrs[i:], quote)
		if end < 0 {
			end = len(attrs) - i
		}
		return attrs[i : i+end], i + end + 1
	}
	start := i
	for i < len(attrs) && !isSpeakSpace(attrs[i]) {
		i++
	}
	return attrs[start:i], i
}

// asciiLower lower-cases an ASCII letter and leaves every other byte alone.
func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// isSpeakSpace reports whether c is ASCII whitespace inside a tag.
func isSpeakSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
