// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package mail

import (
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"
)

func TestBuildMessageHeaders(t *testing.T) {
	m := New(Config{From: "gw@example.com", FromName: "OP Gateway"})
	raw := string(m.buildMessage("bob@example.com", "Betreff", "line1\nline2"))

	for _, want := range []string{
		"From: \"OP Gateway\" <gw@example.com>\r\n",
		"To: bob@example.com\r\n",
		"Subject: Betreff\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("message missing header %q\n---\n%s", want, raw)
		}
	}
	// header/body separator + CRLF body
	if !strings.Contains(raw, "\r\n\r\nline1\r\nline2") {
		t.Fatalf("body not CRLF-normalized after blank line:\n%s", raw)
	}
	if strings.Contains(raw, "line1\nline2") {
		t.Fatalf("bare LF leaked into message:\n%s", raw)
	}
}

func TestBuildMessageBareFromWhenNoName(t *testing.T) {
	m := New(Config{From: "gw@example.com"})
	raw := string(m.buildMessage("b@x", "s", "b"))
	if !strings.Contains(raw, "From: gw@example.com\r\n") {
		t.Fatalf("want bare From address, got:\n%s", raw)
	}
}

func TestBuildMessageQuotedPrintableBody(t *testing.T) {
	m := New(Config{From: "gw@example.com"})
	const body = "Für Sie wurde ein Zugang angelegt.\nGrüße, ÖÄÜ, straße"
	raw := string(m.buildMessage("b@x", "s", body))

	if !strings.Contains(raw, "Content-Transfer-Encoding: quoted-printable\r\n") {
		t.Fatalf("want quoted-printable Content-Transfer-Encoding header:\n%s", raw)
	}
	if strings.Contains(raw, "Content-Transfer-Encoding: 8bit\r\n") {
		t.Fatalf("stale 8bit Content-Transfer-Encoding header:\n%s", raw)
	}

	sep := "\r\n\r\n"
	i := strings.Index(raw, sep)
	if i < 0 {
		t.Fatalf("no header/body separator:\n%s", raw)
	}
	encodedBody := raw[i+len(sep):]

	// The encoded body must be 7-bit clean: no raw UTF-8 continuation bytes
	// (>= 0x80) may appear outside of "=XX" escapes.
	for _, b := range []byte(encodedBody) {
		if b >= 0x80 {
			t.Fatalf("non-ASCII byte 0x%02x leaked into quoted-printable body:\n%q", b, encodedBody)
		}
	}

	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(encodedBody)))
	if err != nil {
		t.Fatalf("decode quoted-printable body: %v", err)
	}
	// normalizeCRLF turns the bare \n in body into \r\n before encoding, so
	// the round-tripped/decoded text is the CRLF-normalized original.
	want := normalizeCRLF(body)
	if string(decoded) != want {
		t.Fatalf("round-trip mismatch:\ngot:  %q\nwant: %q", decoded, want)
	}
}

func TestBuildMessageEncodesNonASCIISubject(t *testing.T) {
	m := New(Config{From: "gw@example.com"})
	raw := string(m.buildMessage("b@x", "Zugang für Sie", "b"))
	// RFC 2047 encoded-word, never the raw UTF-8 bytes, in the header
	if !strings.Contains(raw, "Subject: =?utf-8?") {
		t.Fatalf("non-ASCII subject not RFC2047-encoded:\n%s", raw)
	}
	if strings.Contains(raw, "Subject: Zugang für Sie") {
		t.Fatalf("raw UTF-8 subject leaked into header:\n%s", raw)
	}
}
