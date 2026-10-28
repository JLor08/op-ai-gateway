// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"strings"
	"testing"
)

// These tests drive the <speak lang="..."> stream scanner on its own: it is a pure
// component (no gateway wiring), so every behaviour -- tag stripping, split tags, the
// first-region-only capture, tolerant attribute parsing -- is provable by feeding it
// deltas and reading back what it returns.

// feedSpeakScanner writes every delta in order, then closes the scanner, and returns
// the concatenated clean text plus whether any Write reported the first region closing.
func feedSpeakScanner(s *speakScanner, deltas ...string) (clean string, closed bool) {
	var b strings.Builder
	for _, d := range deltas {
		c, cl := s.Write(d)
		b.WriteString(c)
		closed = closed || cl
	}
	b.WriteString(s.Close())
	return b.String(), closed
}

func TestSpeakScanner_WholeRegionOneWrite(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`<speak lang="de">Kurz.</speak> Detail.`)
	// The clean text keeps summary + detail, the tags are gone.
	if clean != "Kurz. Detail." {
		t.Errorf("clean = %q, want %q", clean, "Kurz. Detail.")
	}
	if !closed {
		t.Error("closed = false, want true: the first region closed on this delta")
	}
	text, lang := s.Summary()
	if text != "Kurz." {
		t.Errorf("summary text = %q, want %q", text, "Kurz.")
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
	if !s.Found() {
		t.Error("Found() = false, want true")
	}
}

func TestSpeakScanner_TagSplitAcrossWrites(t *testing.T) {
	s := newSpeakScanner()
	var clean string
	for _, d := range []string{"<spea", `k lang="d`, `e">Hi`, "</spe", "ak> rest"} {
		c, _ := s.Write(d)
		clean += c
	}
	clean += s.Close()
	if clean != "Hi rest" {
		t.Errorf("clean = %q, want %q", clean, "Hi rest")
	}
	text, lang := s.Summary()
	if text != "Hi" {
		t.Errorf("summary text = %q, want %q", text, "Hi")
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
}

// The close signal is reported on the EXACT delta that completes </speak>, not before and
// not after -- Task 5 fires the speech leg on that delta, so the timing is load-bearing when
// the close tag is split across writes.
func TestSpeakScanner_CloseReportedOnTheCompletingDelta(t *testing.T) {
	s := newSpeakScanner()
	// Open and summary, then the close tag arrives split as "</spe" + "ak>".
	if _, closed := s.Write(`<speak lang="de">Sum.`); closed {
		t.Fatal("closed = true on the open+summary delta, want false")
	}
	if clean, closed := s.Write("</spe"); closed || clean != "" {
		t.Fatalf("partial close delta: clean = %q, closed = %v, want \"\" and false (the tag is incomplete)", clean, closed)
	}
	clean, closed := s.Write("ak> rest")
	if !closed {
		t.Fatal("closed = false on the delta that completes </speak>, want true")
	}
	if clean != " rest" {
		t.Fatalf("clean = %q, want %q (the detail after the close tag)", clean, " rest")
	}
	// A later delta never re-reports the (already closed) first region.
	if _, closed := s.Write(" more"); closed {
		t.Fatal("closed = true on a post-close delta, want false (the first region closes once)")
	}
	if text, lang := s.Summary(); text != "Sum." || lang != "de" {
		t.Fatalf("Summary() = (%q, %q), want (%q, %q)", text, lang, "Sum.", "de")
	}
}

func TestSpeakScanner_HoldsBackOnlyTheTagBytes(t *testing.T) {
	s := newSpeakScanner()
	// Text up to the '<' is published right away; only the possible-tag bytes wait.
	clean, closed := s.Write("Hello <spea")
	if clean != "Hello " {
		t.Errorf("clean = %q, want %q", clean, "Hello ")
	}
	if closed {
		t.Error("closed = true, want false")
	}
	// The held bytes resolve into a real tag: nothing of it ever reaches the clean text.
	clean, _ = s.Write(`k lang="de">Sum`)
	if clean != "Sum" {
		t.Errorf("clean = %q, want %q", clean, "Sum")
	}
	clean, closed = s.Write("</speak>")
	if clean != "" {
		t.Errorf("clean = %q, want empty", clean)
	}
	if !closed {
		t.Error("closed = false, want true")
	}
}

func TestSpeakScanner_NoTag(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := feedSpeakScanner(s, "Just a plain ", "answer, no tags.")
	if clean != "Just a plain answer, no tags." {
		t.Errorf("clean = %q, want the input unchanged", clean)
	}
	if closed {
		t.Error("closed = true, want false")
	}
	if s.Found() {
		t.Error("Found() = true, want false")
	}
	text, lang := s.Summary()
	if text != "" || lang != "" {
		t.Errorf("Summary() = (%q, %q), want empty", text, lang)
	}
}

func TestSpeakScanner_Unclosed(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := feedSpeakScanner(s, `<speak lang="de">partial`)
	if closed {
		t.Error("closed = true, want false: the region was never closed")
	}
	if clean != "partial" {
		t.Errorf("clean = %q, want %q", clean, "partial")
	}
	if !s.Found() {
		t.Error("Found() = false, want true")
	}
	// An unclosed first region's captured text is still the summary.
	text, lang := s.Summary()
	if text != "partial" {
		t.Errorf("summary text = %q, want %q", text, "partial")
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
}

func TestSpeakScanner_EmptyRegion(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`<speak lang="de"></speak>x`)
	if clean != "x" {
		t.Errorf("clean = %q, want %q", clean, "x")
	}
	if !closed {
		t.Error("closed = false, want true")
	}
	if !s.Found() {
		t.Error("Found() = false, want true")
	}
	text, lang := s.Summary()
	if text != "" {
		t.Errorf("summary text = %q, want empty", text)
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
}

func TestSpeakScanner_FirstRegionOnly(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`<speak lang="de">Eins.</speak> Mitte <speak lang="en">Two.</speak> Ende.`)
	if clean != "Eins. Mitte Two. Ende." {
		t.Errorf("clean = %q, want %q", clean, "Eins. Mitte Two. Ende.")
	}
	if !closed {
		t.Error("closed = false, want true")
	}
	// The summary is the first region only; the second region's text stays in the clean
	// stream as plain detail, and its lang does not replace the first one.
	text, lang := s.Summary()
	if text != "Eins." {
		t.Errorf("summary text = %q, want %q", text, "Eins.")
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
}

func TestSpeakScanner_SecondRegionDoesNotReportClosed(t *testing.T) {
	s := newSpeakScanner()
	if _, closed := s.Write(`<speak lang="de">A</speak>`); !closed {
		t.Fatal("first region: closed = false, want true")
	}
	// Only the FIRST region's close is reported; later regions are stripped silently.
	clean, closed := s.Write(`<speak lang="en">B</speak>`)
	if closed {
		t.Error("second region: closed = true, want false")
	}
	if clean != "B" {
		t.Errorf("clean = %q, want %q", clean, "B")
	}
}

func TestSpeakScanner_AttributeRobustness(t *testing.T) {
	tests := []struct {
		name     string
		open     string
		wantLang string
	}{
		{"double quotes", `<speak lang="de">`, "de"},
		{"single quotes", `<speak lang='de'>`, "de"},
		{"unquoted value", `<speak lang=de>`, "de"},
		{"extra attribute before", `<speak id="s1" lang="de">`, "de"},
		{"extra attribute after", `<speak lang="de" voice="x">`, "de"},
		{"inner spaces around equals", `<speak   lang  =  "de"  >`, "de"},
		{"newline between attributes", "<speak\nid=\"s1\"\nlang=\"de\">", "de"},
		{"mixed case tag and name", `<SpEaK LaNg="de">`, "de"},
		{"bcp47 value verbatim", `<speak lang="pt-BR">`, "pt-BR"},
		{"value not normalized", `<speak lang="DE">`, "DE"},
		{"lang inside another value", `<speak note="lang='fr'" lang="de">`, "de"},
		{"gt inside a quoted value", `<speak note="a>b" lang="de">`, "de"},
		{"no lang attribute", `<speak id="s1">`, ""},
		{"no attributes", `<speak>`, ""},
		{"empty lang value", `<speak lang="">`, ""},
	}
	closers := []string{"</speak>", "</SPEAK>", "</speak >"}
	for _, tc := range tests {
		for _, closer := range closers {
			t.Run(tc.name+" / "+closer, func(t *testing.T) {
				s := newSpeakScanner()
				clean, closed := feedSpeakScanner(s, "Vorab ", tc.open, "Zusammenfassung.", closer, " Detail.")
				if clean != "Vorab Zusammenfassung. Detail." {
					t.Errorf("clean = %q, want %q", clean, "Vorab Zusammenfassung. Detail.")
				}
				if !closed {
					t.Error("closed = false, want true")
				}
				text, lang := s.Summary()
				if text != "Zusammenfassung." {
					t.Errorf("summary text = %q, want %q", text, "Zusammenfassung.")
				}
				if lang != tc.wantLang {
					t.Errorf("summary lang = %q, want %q", lang, tc.wantLang)
				}
			})
		}
	}
}

func TestSpeakScanner_LeadingTextBeforeTag(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`pre <speak lang="de">sum</speak>`)
	if clean != "pre sum" {
		t.Errorf("clean = %q, want %q", clean, "pre sum")
	}
	if !closed {
		t.Error("closed = false, want true")
	}
	text, lang := s.Summary()
	if text != "sum" {
		t.Errorf("summary text = %q, want %q", text, "sum")
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
}

func TestSpeakScanner_NoLangAttr(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write("<speak>sum</speak>")
	if clean != "sum" {
		t.Errorf("clean = %q, want %q", clean, "sum")
	}
	if !closed {
		t.Error("closed = false, want true")
	}
	text, lang := s.Summary()
	if text != "sum" {
		t.Errorf("summary text = %q, want %q", text, "sum")
	}
	if lang != "" {
		t.Errorf("summary lang = %q, want empty", lang)
	}
}

func TestSpeakScanner_OtherAngleBracketsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"less-than sign in prose", "if a < b and c > d then"},
		{"other html tag", "use <b>bold</b> sparingly"},
		{"longer tag name sharing the prefix", "the <speaker> and </speakers> stay"},
		{"generic type", "a List<String> value"},
		{"lone closing tag lookalike", "x </spea y"},
		{"unterminated tag at end", "tail <"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpeakScanner()
			clean, closed := feedSpeakScanner(s, tc.in)
			if clean != tc.in {
				t.Errorf("clean = %q, want the input unchanged %q", clean, tc.in)
			}
			if closed {
				t.Error("closed = true, want false")
			}
			if s.Found() {
				t.Error("Found() = true, want false")
			}
		})
	}
}

func TestSpeakScanner_CloseFlushesAmbiguousPartial(t *testing.T) {
	s := newSpeakScanner()
	clean, _ := s.Write("done <spea")
	if clean != "done " {
		t.Fatalf("clean = %q, want %q", clean, "done ")
	}
	// The stream ends mid-tag: the held bytes were never a tag, so they come out as text.
	if got := s.Close(); got != "<spea" {
		t.Errorf("Close() = %q, want %q", got, "<spea")
	}
	if s.Found() {
		t.Error("Found() = true, want false")
	}
	// Close is idempotent: nothing is emitted twice.
	if got := s.Close(); got != "" {
		t.Errorf("second Close() = %q, want empty", got)
	}
}

func TestSpeakScanner_CloseFlushesPartialInsideUnclosedRegion(t *testing.T) {
	s := newSpeakScanner()
	clean, _ := feedSpeakScanner(s, `<speak lang="de">half </spea`)
	if clean != "half </spea" {
		t.Errorf("clean = %q, want %q", clean, "half </spea")
	}
	// The flushed bytes reach the clean stream, but a half-written close tag is markup, not
	// speech: the summary must not see it (see TestSpeakScanner_CloseDropsPartialCloseTagFromSummary).
	if text, _ := s.Summary(); text != "half " {
		t.Errorf("summary text = %q, want %q", text, "half ")
	}
}

func TestSpeakScanner_OverlongOpenTagIsPlainText(t *testing.T) {
	s := newSpeakScanner()
	// An open tag that never terminates must not hold the stream back forever: past the
	// bound it is not a tag after all and is published as ordinary text.
	in := `<speak lang="de" ` + strings.Repeat("x", maxSpeakTagLen*2)
	clean, closed := feedSpeakScanner(s, in)
	if clean != in {
		t.Errorf("clean differs from the input (len %d vs %d)", len(clean), len(in))
	}
	if closed || s.Found() {
		t.Errorf("closed=%v Found=%v, want both false", closed, s.Found())
	}

	// The bound is on the tag's length, not on whether it eventually terminates: a tag
	// longer than the bound is plain text even when its '>' does arrive. The trailing
	// close tag is still a real one and is stripped, but it closes no region.
	s = newSpeakScanner()
	overlong := `<speak lang="de" ` + strings.Repeat("x", maxSpeakTagLen*2) + `>tail`
	clean, closed = feedSpeakScanner(s, overlong+"</speak>")
	if clean != overlong {
		t.Errorf("terminated overlong tag: clean differs from the input (len %d vs %d)", len(clean), len(overlong))
	}
	if closed || s.Found() {
		t.Errorf("terminated overlong tag: closed=%v Found=%v, want both false", closed, s.Found())
	}
}

func TestSpeakScanner_NestedOpenInsideFirstRegionIsStripped(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`<speak lang="de">a <speak lang="en">b</speak> c`)
	if clean != "a b c" {
		t.Errorf("clean = %q, want %q", clean, "a b c")
	}
	if !closed {
		t.Error("closed = false, want true: the first close tag ends the region")
	}
	text, lang := s.Summary()
	if text != "a b" || lang != "de" {
		t.Errorf("Summary() = (%q, %q), want (%q, %q)", text, lang, "a b", "de")
	}
}

func TestSpeakScanner_MultibyteTextKeptIntact(t *testing.T) {
	s := newSpeakScanner()
	clean, _ := feedSpeakScanner(s, `<speak lang="de">Grüße `, "aus Köln</speak> – schön.")
	if clean != "Grüße aus Köln – schön." {
		t.Errorf("clean = %q, want %q", clean, "Grüße aus Köln – schön.")
	}
	if text, _ := s.Summary(); text != "Grüße aus Köln" {
		t.Errorf("summary text = %q, want %q", text, "Grüße aus Köln")
	}
}

// TestSpeakScanner_ChunkingIsInvisible proves the scanner's result does not depend on how
// the stream happens to be cut into deltas: for every input, feeding it whole, one byte at
// a time, and in every two-way split yields the same clean text, summary and close report.
func TestSpeakScanner_ChunkingIsInvisible(t *testing.T) {
	inputs := []string{
		`<speak lang="de">Kurz.</speak> Detail.`,
		`pre <speak lang='en'>sum</speak> post`,
		`<SPEAK id="1"  lang = "fr" >Résumé</SPEAK ><speak lang="de">zwei</speak>`,
		`<speak lang="de">unclosed </spea`,
		`no tags < here <b>x</b> <speaker>`,
		`<speak></speak>x`,
		`<speak lang="de" ` + strings.Repeat("x", maxSpeakTagLen) + `>over the bound</speak> tail`,
	}
	for _, in := range inputs {
		whole := newSpeakScanner()
		wantClean, wantClosed := feedSpeakScanner(whole, in)
		wantText, wantLang := whole.Summary()

		check := func(label string, deltas []string) {
			t.Helper()
			s := newSpeakScanner()
			clean, closed := feedSpeakScanner(s, deltas...)
			text, lang := s.Summary()
			if clean != wantClean || closed != wantClosed || text != wantText || lang != wantLang || s.Found() != whole.Found() {
				t.Errorf("%s on %q: got (%q, %v, %q, %q, %v), want (%q, %v, %q, %q, %v)", label, in,
					clean, closed, text, lang, s.Found(), wantClean, wantClosed, wantText, wantLang, whole.Found())
			}
		}

		bytewise := make([]string, 0, len(in))
		for i := range len(in) {
			bytewise = append(bytewise, in[i:i+1])
		}
		check("byte-at-a-time", bytewise)
		for cut := 1; cut < len(in); cut++ {
			check("split", []string{in[:cut], in[cut:]})
		}
	}
}

// A tag whose '>' is immediately preceded by '/' is self-closing: it is stripped and counts
// as seen (Found, lang) but it opens no spoken region, so it captures nothing and never
// swallows the rest of the stream.
func TestSpeakScanner_SelfClosingTagOpensNoRegion(t *testing.T) {
	tests := []struct {
		name     string
		tag      string
		wantLang string
	}{
		{"bare", `<speak/>`, ""},
		{"space before slash", `<speak />`, ""},
		{"with lang", `<speak lang="de"/>`, "de"},
		{"with lang and space before slash", `<speak lang="de" />`, "de"},
		{"single quoted lang", `<speak lang='en'/>`, "en"},
		{"unquoted lang", `<speak lang=de/>`, "de"},
		{"mixed case", `<SPEAK Lang="fr"/>`, "fr"},
		{"newline before slash", "<speak\n/>", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpeakScanner()
			clean, closed := feedSpeakScanner(s, "Vorab ", tc.tag, " Antwort.")
			if clean != "Vorab  Antwort." {
				t.Errorf("clean = %q, want %q (the self-closing tag stripped, everything else kept)", clean, "Vorab  Antwort.")
			}
			if closed {
				t.Error("closed = true, want false: a self-closing tag has no region to close")
			}
			if !s.Found() {
				t.Error("Found() = false, want true")
			}
			text, lang := s.Summary()
			if text != "" {
				t.Errorf("summary text = %q, want empty: no region was opened", text)
			}
			if lang != tc.wantLang {
				t.Errorf("summary lang = %q, want %q", lang, tc.wantLang)
			}
		})
	}
}

// A self-closing tag is not a region: it must not turn the rest of the stream into a summary,
// and a stray </speak> after it still closes nothing.
func TestSpeakScanner_SelfClosingTagDoesNotConsumeTheStream(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`<speak />Alles bleibt Detail.</speak> Ende.`)
	if clean != "Alles bleibt Detail. Ende." {
		t.Errorf("clean = %q, want %q", clean, "Alles bleibt Detail. Ende.")
	}
	if closed {
		t.Error("closed = true, want false")
	}
	if text, _ := s.Summary(); text != "" {
		t.Errorf("summary text = %q, want empty", text)
	}
}

// Once the first region exists a self-closing tag is irrelevant: inside the region it is
// stripped like any nested tag and neither ends the region nor changes its lang, and after
// the region it is stripped without touching the captured summary.
func TestSpeakScanner_SelfClosingTagAfterFirstRegionIsIrrelevant(t *testing.T) {
	s := newSpeakScanner()
	clean, closed := s.Write(`<speak lang="de">a <speak lang="en"/> b</speak> c <speak/> d`)
	if clean != "a  b c  d" {
		t.Errorf("clean = %q, want %q", clean, "a  b c  d")
	}
	if !closed {
		t.Error("closed = false, want true: the real close tag ends the region")
	}
	text, lang := s.Summary()
	if text != "a  b" || lang != "de" {
		t.Errorf("Summary() = (%q, %q), want (%q, %q)", text, lang, "a  b", "de")
	}
}

// A '/' that is not directly followed by '>' is no self-closing marker: the lookalike stays
// plain text, and one cut off at the end of the stream is flushed as text.
func TestSpeakScanner_SlashNotBeforeGtIsPlainText(t *testing.T) {
	for _, in := range []string{"a <speak/x> b", "a <speaker/> b", "a <speak/"} {
		s := newSpeakScanner()
		clean, closed := feedSpeakScanner(s, in)
		if clean != in {
			t.Errorf("clean of %q = %q, want the input unchanged", in, clean)
		}
		if closed || s.Found() {
			t.Errorf("%q: closed=%v Found=%v, want both false", in, closed, s.Found())
		}
	}
}

// When the stream ends inside the first region while a close tag is only half written, the
// held "</spea" is markup, not speech: the clean stream still gets it (held bytes flush as
// plain text), but the spoken summary must not.
func TestSpeakScanner_CloseDropsPartialCloseTagFromSummary(t *testing.T) {
	tests := []struct {
		name      string
		partial   string
		wantClean string
	}{
		{"lt slash", "</", "half </"},
		{"through the name minus one", "</spea", "half </spea"},
		{"through the name", "</speak", "half </speak"},
		{"mixed case", "</SPEA", "half </SPEA"},
		{"name and whitespace", "</speak ", "half </speak "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpeakScanner()
			clean, closed := feedSpeakScanner(s, `<speak lang="de">half `, tc.partial)
			if clean != tc.wantClean {
				t.Errorf("clean = %q, want %q", clean, tc.wantClean)
			}
			if closed {
				t.Error("closed = true, want false: the close tag never completed")
			}
			text, lang := s.Summary()
			if text != "half " {
				t.Errorf("summary text = %q, want %q: no close-tag fragment in the spoken text", text, "half ")
			}
			if lang != "de" {
				t.Errorf("summary lang = %q, want %q", lang, "de")
			}
		})
	}
}

// Only a half-written CLOSE tag is dropped from the summary at stream end: held bytes that
// are not one (a lone '<', a half-written open tag) are still region content, and the same
// partial close tag outside the first region never touches the summary at all.
func TestSpeakScanner_CloseKeepsOtherPartialsInSummary(t *testing.T) {
	for _, partial := range []string{"<", "<spea", "<speak lang=\"e"} {
		s := newSpeakScanner()
		clean, _ := feedSpeakScanner(s, `<speak>half `, partial)
		if want := "half " + partial; clean != want {
			t.Errorf("clean = %q, want %q", clean, want)
		}
		if text, _ := s.Summary(); text != "half "+partial {
			t.Errorf("summary text = %q, want %q", text, "half "+partial)
		}
	}

	// After the region closed, a trailing partial close tag is detail text, not summary.
	s := newSpeakScanner()
	clean, _ := feedSpeakScanner(s, `<speak>sum</speak> detail </spea`)
	if clean != "sum detail </spea" {
		t.Errorf("clean = %q, want %q", clean, "sum detail </spea")
	}
	if text, _ := s.Summary(); text != "sum" {
		t.Errorf("summary text = %q, want %q", text, "sum")
	}
}

// TestSpeakScanner_SelfClosingChunkingIsInvisible is the chunk-invariance proof for the
// self-closing and stream-end-in-a-partial-close-tag paths: whole, byte-at-a-time and every
// two-way split must agree on clean text, summary, lang, found and the close report.
func TestSpeakScanner_SelfClosingChunkingIsInvisible(t *testing.T) {
	inputs := []string{
		`<speak/>x`,
		`<speak />x`,
		`<speak lang="de"/>x</speak> y`,
		`<speak lang="de" />x`,
		`a <speak/> b <speak lang="de">c</speak> d`,
		`<speak lang="de">a <speak/> b</speak> c`,
		`<speak/x> not a tag <speak/`,
		`<speak lang="de">unclosed </speak`,
		`<speak lang="de">unclosed </speak `,
		`<speak lang="de">unclosed </`,
		`<speak lang="de" ` + strings.Repeat("x", maxSpeakTagLen) + `/>over the bound`,
	}
	for _, in := range inputs {
		whole := newSpeakScanner()
		wantClean, wantClosed := feedSpeakScanner(whole, in)
		wantText, wantLang := whole.Summary()

		check := func(label string, deltas []string) {
			t.Helper()
			s := newSpeakScanner()
			clean, closed := feedSpeakScanner(s, deltas...)
			text, lang := s.Summary()
			if clean != wantClean || closed != wantClosed || text != wantText || lang != wantLang || s.Found() != whole.Found() {
				t.Errorf("%s on %q: got (%q, %v, %q, %q, %v), want (%q, %v, %q, %q, %v)", label, in,
					clean, closed, text, lang, s.Found(), wantClean, wantClosed, wantText, wantLang, whole.Found())
			}
		}

		bytewise := make([]string, 0, len(in))
		for i := range len(in) {
			bytewise = append(bytewise, in[i:i+1])
		}
		check("byte-at-a-time", bytewise)
		for cut := 1; cut < len(in); cut++ {
			check("split", []string{in[:cut], in[cut:]})
		}
	}
}
