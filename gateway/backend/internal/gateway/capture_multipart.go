// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"strings"
	"unicode/utf8"
)

// captureTextPartMaxBytes is the largest part captureRequestBody will take for a
// text field. The edit handler refuses a field past 1 MiB (maxImageEditFieldBytes),
// so a bigger part is not one of the fields -- and is redacted rather than read.
const captureTextPartMaxBytes = maxImageEditFieldBytes

// captureMultipartPart is one entry of the stored summary of a multipart body:
// what the part is called and says it is, how large it is, and -- only for a text
// field -- its value. A part that is binary (or claims to be a file) carries
// Redacted instead of content.
type captureMultipartPart struct {
	Name        string  `json:"name"`
	Filename    string  `json:"filename,omitempty"`
	ContentType string  `json:"content_type,omitempty"`
	Size        int64   `json:"size"`
	Text        *string `json:"text,omitempty"`
	Redacted    bool    `json:"redacted,omitempty"`
	Truncated   bool    `json:"truncated,omitempty"`
}

// captureMultipartSummary is the JSON document stored as the request body of a
// multipart capture (the capture viewer pretty-prints it like any JSON body).
type captureMultipartSummary struct {
	Multipart string                 `json:"multipart"`
	Size      int                    `json:"size"`
	Parts     []captureMultipartPart `json:"parts,omitempty"`
	// Error says why the parts end where they do: a body that could not be walked
	// is never stored raw instead.
	Error string `json:"error,omitempty"`
}

// captureRequestBody returns what the envelope stores as the request body, and
// whether that was cut short. For multipart/form-data it is the summary of
// summarizeMultipartBody; for everything else it is the body itself clipped to
// maxBytes, exactly as before.
func captureRequestBody(in *captureInput, maxBytes int) (string, bool) {
	if mediaType, params, _ := mime.ParseMediaType(in.ReqHeaders.Get("Content-Type")); mediaType == "multipart/form-data" {
		return summarizeMultipartBody(in.RawReq, params["boundary"], maxBytes)
	}
	body, truncated := capBody(in.RawReq, maxBytes)
	return string(body), truncated
}

// summarizeMultipartBody walks a multipart/form-data body once and returns the
// JSON summary stored in its place: every part's name, file name, type and size,
// the VALUE of each text field (a part with no file name, no non-text type and
// valid UTF-8 content), and nothing of any other part's content. The text values
// share maxBytes (<= 0 disables the cap) and are cut on a rune boundary, which is
// what makes the second result -- truncated -- true. The summary is built from the
// whole body, so a large binary part never costs the capture its small text ones.
//
// It never falls back to the raw bytes. A body with no usable boundary, or one
// that breaks off part-way, yields a summary that says so (Error) and carries the
// parts read before the break.
func summarizeMultipartBody(raw []byte, boundary string, maxBytes int) (string, bool) {
	doc := captureMultipartSummary{Multipart: "form-data", Size: len(raw)}
	truncated := false
	if boundary == "" {
		doc.Error = "the body was not read: its Content-Type names no multipart boundary; its content is redacted"
		return encodeMultipartSummary(doc), false
	}
	budget := math.MaxInt
	if maxBytes > 0 {
		budget = maxBytes
	}
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			doc.Error = "malformed multipart body: the parts end where it breaks off"
			break
		}
		p, head, err := readCapturePart(part)
		if err != nil {
			doc.Error = "malformed multipart body: the parts end where it breaks off"
			break
		}
		used, cut := applyCaptureText(&p, head, budget)
		budget -= used
		truncated = truncated || cut
		doc.Parts = append(doc.Parts, p)
	}
	return encodeMultipartSummary(doc), truncated
}

// readCapturePart reads one part to its end, as much of it as the summary needs
// kept: the part's name, file name, type and size, and -- only when it presents
// itself as a text field (declaresATextField) -- up to captureTextPartMaxBytes+1 of
// its leading bytes (head; nil for any other part). Whatever was not kept is still
// read, and counted in Size, so the walk reaches the next part. The error is the
// first failure of either read: the part broke off.
func readCapturePart(part *multipart.Part) (p captureMultipartPart, head []byte, err error) {
	p = captureMultipartPart{Name: part.FormName(), Filename: part.FileName(), ContentType: part.Header.Get("Content-Type")}
	var readErr error
	if declaresATextField(p) {
		head, readErr = io.ReadAll(io.LimitReader(part, captureTextPartMaxBytes+1))
	}
	rest, drainErr := io.Copy(io.Discard, part)
	p.Size = int64(len(head)) + rest
	return p, head, errors.Join(readErr, drainErr)
}

// applyCaptureText decides what p carries of its content: the value of a text field
// -- head, when there is one, within captureTextPartMaxBytes and valid UTF-8 -- cut
// on a rune boundary to fit budget (the room left for the text values of the whole
// summary), or nothing, in which case the part is marked Redacted. It returns how
// much of the budget the value used and whether it had to be cut.
func applyCaptureText(p *captureMultipartPart, head []byte, budget int) (used int, cut bool) {
	if head == nil || len(head) > captureTextPartMaxBytes || !utf8.Valid(head) {
		p.Redacted = true
		return 0, false
	}
	text := string(head)
	if len(text) > budget {
		text, cut = text[:runeCutPoint(text, budget)], true
		p.Truncated = true
	}
	p.Text = &text
	return len(text), cut
}

// runeCutPoint is the largest index at or below limit (which must be inside s) at
// which s can be cut without splitting a rune.
func runeCutPoint(s string, limit int) int {
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return limit
}

// declaresATextField reports whether a part presents itself as a plain form field:
// no file name, and no content type other than text/*. (It may still turn out to
// be binary; the content is checked too.)
func declaresATextField(p captureMultipartPart) bool {
	if p.Filename != "" {
		return false
	}
	return p.ContentType == "" || strings.HasPrefix(strings.ToLower(p.ContentType), "text/")
}

// encodeMultipartSummary marshals doc without HTML escaping, so a prompt's
// <sd_cpp_extra_args> block reads as written in the raw view of a capture.
func encodeMultipartSummary(doc captureMultipartSummary) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return `{"multipart":"form-data","error":"the summary could not be encoded; the body's content is redacted"}`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
