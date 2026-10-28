// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "strings"

// maxSpeakTagLen bounds how many bytes the scanner will hold back while it decides
// whether a '<' starts a <speak ...> / </speak> tag. A real tag is far shorter; a tag
// that is not complete within this many bytes is treated as ordinary text, so a stray
// "<speak " in the model output can never stall the published stream.
const maxSpeakTagLen = 512

// speakScanner extracts the first <speak lang="..."> ... </speak> region from an LLM token
// stream (the Voice Chat spoken summary) while stripping every speak tag from the text
// that is published to the transcript.
//
// The stream is fed delta by delta through Write. Text outside a tag is published
// immediately; only the bytes of a possible tag that is still being parsed are held back
// until the next delta resolves them (or Close flushes them as plain text). Both the
// summary and the detail text pass through the clean stream -- the tags are the only
// thing removed. Only the first region is captured: later <speak> tags are stripped too,
// but their text stays in the clean stream as ordinary detail.
//
// The scanner works on bytes. '<' is ASCII and never part of a multi-byte UTF-8 sequence,
// so a rune split across two deltas is passed through untouched like any other text.
// It is not safe for concurrent use; one scanner serves one stream.
type speakScanner struct {
	// pending holds the bytes of a possible tag (always starting with '<') that the last
	// delta ended in the middle of.
	pending string
	// found is set once any <speak> open tag has been seen; the first one starts the
	// captured region and later ones are only stripped.
	found bool
	// inFirst is true while the stream is between the first open tag and its close tag.
	inFirst bool
	// lang is the first open tag's lang attribute, verbatim.
	lang string
	// summary accumulates the first region's text.
	summary strings.Builder
}

// newSpeakScanner returns a scanner ready for the start of a stream.
func newSpeakScanner() *speakScanner {
	return &speakScanner{}
}

// Write feeds one raw content delta. It returns the tag-stripped text that is safe to
// publish now, and whether the first <speak> region closed on this delta.
func (s *speakScanner) Write(delta string) (clean string, closed bool) {
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
			return out.String(), closed
		case speakTagOpen:
			s.openTag(attrs)
			in = in[n:]
		case speakTagSelfClosing:
			s.selfClosingTag(attrs)
			in = in[n:]
		case speakTagClose:
			if s.closeTag() {
				closed = true
			}
			in = in[n:]
		default:
			// Not a speak tag: the '<' is ordinary text. Rescan from the next byte so a
			// '<' later in the same run is still considered.
			s.emit(&out, "<")
			in = in[1:]
		}
	}
	return out.String(), closed
}

// Close flushes the scanner at the end of the stream. Bytes held back for a tag that
// never completed are emitted as plain text. The one exception is the spoken summary: a
// half-written </speak> at the end of a still-open first region is markup, not speech, so
// those bytes reach the clean stream but are kept out of the captured summary.
func (s *speakScanner) Close() (clean string) {
	pending := s.pending
	s.pending = ""
	if s.inFirst && isPartialSpeakClose(pending) {
		return pending
	}
	var out strings.Builder
	s.emit(&out, pending)
	return out.String()
}

// Summary returns the first region's captured text and its lang attribute. When the
// region was never closed it is the full text captured so far; when no <speak> tag was
// seen both are empty.
func (s *speakScanner) Summary() (text string, lang string) {
	return s.summary.String(), s.lang
}

// Found reports whether any <speak> open tag was seen, a self-closing one included.
func (s *speakScanner) Found() bool {
	return s.found
}

// emit publishes text to the clean stream, and also to the summary while inside the
// first region.
func (s *speakScanner) emit(out *strings.Builder, text string) {
	if text == "" {
		return
	}
	out.WriteString(text)
	if s.inFirst {
		s.summary.WriteString(text)
	}
}

// openTag handles a <speak ...> tag. Only the first one starts a captured region; any
// later one (a second region, or a nested tag inside the first) is merely stripped.
func (s *speakScanner) openTag(attrs string) {
	if s.found {
		return
	}
	s.found = true
	s.inFirst = true
	s.lang = parseSpeakLang(attrs)
}

// selfClosingTag handles a <speak .../> tag. It is stripped and counts as a speak tag that
// was seen (Found, lang), but it opens no region: there is no text to capture and the rest of
// the stream stays ordinary text. Like any open tag, only the first one seen records its lang,
// so a self-closing tag after the first region is merely stripped.
func (s *speakScanner) selfClosingTag(attrs string) {
	if s.found {
		return
	}
	s.found = true
	s.lang = parseSpeakLang(attrs)
}

// closeTag handles a </speak> tag and reports whether it closed the first region.
func (s *speakScanner) closeTag() bool {
	if !s.inFirst {
		return false
	}
	s.inFirst = false
	return true
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
