// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"slices"
	"strings"
	"testing"
)

// These tests drive the <speak lang="..."> stream scanner on its own: it is a pure
// component (no gateway wiring), so every behaviour -- tag stripping, split tags, the
// ordered capture of every region, tolerant attribute parsing -- is provable by feeding it
// deltas and reading back what it returns.

// feedSpeakScanner writes every delta in order, then closes the scanner, and returns
// the concatenated clean text plus whether any Write reported a region closing.
func feedSpeakScanner(s *speakScanner, deltas ...string) (clean string, closed bool) {
	var b strings.Builder
	for _, d := range deltas {
		c, _, cl := s.Write(d)
		b.WriteString(c)
		closed = closed || cl
	}
	b.WriteString(s.Close())
	return b.String(), closed
}

func TestSpeakScanner_WholeRegionOneWrite(t *testing.T) {
	s := newSpeakScanner()
	clean, _, closed := s.Write(`<speak lang="de">Kurz.</speak> Detail.`)
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
		c, _, _ := s.Write(d)
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
	if _, _, closed := s.Write(`<speak lang="de">Sum.`); closed {
		t.Fatal("closed = true on the open+summary delta, want false")
	}
	if clean, _, closed := s.Write("</spe"); closed || clean != "" {
		t.Fatalf("partial close delta: clean = %q, closed = %v, want \"\" and false (the tag is incomplete)", clean, closed)
	}
	clean, _, closed := s.Write("ak> rest")
	if !closed {
		t.Fatal("closed = false on the delta that completes </speak>, want true")
	}
	if clean != " rest" {
		t.Fatalf("clean = %q, want %q (the detail after the close tag)", clean, " rest")
	}
	// A later delta never re-reports the (already closed) first region.
	if _, _, closed := s.Write(" more"); closed {
		t.Fatal("closed = true on a post-close delta, want false (the first region closes once)")
	}
	if text, lang := s.Summary(); text != "Sum." || lang != "de" {
		t.Fatalf("Summary() = (%q, %q), want (%q, %q)", text, lang, "Sum.", "de")
	}
}

func TestSpeakScanner_HoldsBackOnlyTheTagBytes(t *testing.T) {
	s := newSpeakScanner()
	// Text up to the '<' is published right away; only the possible-tag bytes wait.
	clean, _, closed := s.Write("Hello <spea")
	if clean != "Hello " {
		t.Errorf("clean = %q, want %q", clean, "Hello ")
	}
	if closed {
		t.Error("closed = true, want false")
	}
	// The held bytes resolve into a real tag: nothing of it ever reaches the clean text.
	clean, _, _ = s.Write(`k lang="de">Sum`)
	if clean != "Sum" {
		t.Errorf("clean = %q, want %q", clean, "Sum")
	}
	clean, _, closed = s.Write("</speak>")
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
	clean, _, closed := s.Write(`<speak lang="de"></speak>x`)
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
	clean, _, closed := s.Write(`<speak lang="de">Eins.</speak> Mitte <speak lang="en">Two.</speak> Ende.`)
	if clean != "Eins. Mitte Two. Ende." {
		t.Errorf("clean = %q, want %q", clean, "Eins. Mitte Two. Ende.")
	}
	if !closed {
		t.Error("closed = false, want true")
	}
	// Summary() is the back-compat accessor for the FIRST region: the second region's text
	// is still in the clean stream, and its lang does not replace the first one. The second
	// region itself is captured too (see Regions and TestSpeakScanner_MultipleRegions).
	text, lang := s.Summary()
	if text != "Eins." {
		t.Errorf("summary text = %q, want %q", text, "Eins.")
	}
	if lang != "de" {
		t.Errorf("summary lang = %q, want %q", lang, "de")
	}
	if got := len(s.Regions()); got != 2 {
		t.Errorf("len(Regions()) = %d, want 2", got)
	}
}

// Every region reports its own close, with the index of the region that closed. This is the
// phase-3 contract that replaces the phase-2 "only the first region reports closed" pin.
func TestSpeakScanner_SecondRegionReportsClosed(t *testing.T) {
	s := newSpeakScanner()
	if _, idx, closed := s.Write(`<speak lang="de">A</speak>`); !closed || idx != 0 {
		t.Fatalf("first region: closed = %v, closedIdx = %d, want true and 0", closed, idx)
	}
	clean, idx, closed := s.Write(`<speak lang="en">B</speak>`)
	if !closed || idx != 1 {
		t.Errorf("second region: closed = %v, closedIdx = %d, want true and 1", closed, idx)
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
	clean, _, closed := s.Write(`pre <speak lang="de">sum</speak>`)
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
	clean, _, closed := s.Write("<speak>sum</speak>")
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
	clean, _, _ := s.Write("done <spea")
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
	clean, _, closed := s.Write(`<speak lang="de">a <speak lang="en">b</speak> c`)
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
	clean, _, closed := s.Write(`<speak />Alles bleibt Detail.</speak> Ende.`)
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
	clean, _, closed := s.Write(`<speak lang="de">a <speak lang="en"/> b</speak> c <speak/> d`)
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

// SummaryStart reports the RUNE offset of the first <speak> region's content within the
// accumulated clean text, so the portal can slice the spoken summary out of the full answer
// by position rather than re-finding it by value (issue #322). It is -1 when no region was
// captured (no tag, or a self-closing tag that opens none), and counts code points -- NOT
// bytes -- so a multi-byte char in the leading detail does not throw the offset off.
func TestSpeakScanner_SummaryStart(t *testing.T) {
	t.Run("zero when the region is the leading text", func(t *testing.T) {
		s := newSpeakScanner()
		feedSpeakScanner(s, `<speak lang="de">Kurz.</speak> Detail.`)
		if got := s.SummaryStart(); got != 0 {
			t.Errorf("SummaryStart() = %d, want 0", got)
		}
	})

	t.Run("points past leading detail, in runes not bytes", func(t *testing.T) {
		// Leading detail "Käse 🧀 " precedes the tag: 'ä' is two bytes and '🧀' is four, so a
		// byte offset would overshoot. The region's content starts at rune 7.
		s := newSpeakScanner()
		clean, _ := feedSpeakScanner(s, `Käse 🧀 <speak lang="de">Kurz.</speak> Detail.`)
		const want = 7
		got := s.SummaryStart()
		if got != want {
			t.Fatalf("SummaryStart() = %d, want %d (runes)", got, want)
		}
		// The offset is EXACT: slicing the clean text's runes at it yields the summary.
		runes := []rune(clean)
		text, _ := s.Summary()
		if string(runes[got:got+len([]rune(text))]) != text {
			t.Errorf("runes[%d:] = %q, want the summary %q", got, string(runes[got:]), text)
		}
	})

	t.Run("points at the real region when the summary repeats in leading detail", func(t *testing.T) {
		// The prompt forbids leading text, but the model may ignore it: here the summary "Kurz."
		// appears BEFORE the tag too. The offset must point at the real <speak> region (rune 6),
		// not the earlier verbatim occurrence (rune 0), which is where indexOf would mis-cut.
		s := newSpeakScanner()
		feedSpeakScanner(s, `Kurz. <speak lang="de">Kurz.</speak> Detail.`)
		if got := s.SummaryStart(); got != 6 {
			t.Errorf("SummaryStart() = %d, want 6 (the real region, not the leading duplicate at 0)", got)
		}
	})

	t.Run("minus one when no tag was seen", func(t *testing.T) {
		s := newSpeakScanner()
		feedSpeakScanner(s, "Just plain text, no tag.")
		if got := s.SummaryStart(); got != -1 {
			t.Errorf("SummaryStart() = %d, want -1", got)
		}
	})

	t.Run("minus one for a self-closing tag that opens no region", func(t *testing.T) {
		s := newSpeakScanner()
		feedSpeakScanner(s, `a <speak lang="de"/> b`)
		if !s.Found() {
			t.Fatal("Found() = false, want true: a self-closing tag was seen")
		}
		if got := s.SummaryStart(); got != -1 {
			t.Errorf("SummaryStart() = %d, want -1: a self-closing tag captures no region", got)
		}
	})

	t.Run("offset is invariant to how the stream is chunked", func(t *testing.T) {
		const in = `Käse 🧀 before <speak lang="de">Kurz.</speak> Detail.`
		whole := newSpeakScanner()
		feedSpeakScanner(whole, in)
		want := whole.SummaryStart()
		for cut := 1; cut < len(in); cut++ {
			s := newSpeakScanner()
			feedSpeakScanner(s, in[:cut], in[cut:])
			if got := s.SummaryStart(); got != want {
				t.Errorf("split at %d: SummaryStart() = %d, want %d", cut, got, want)
			}
		}
	})
}

// runesOf returns the rune-count of s, the unit RuneOffset is measured in.
func runesOf(s string) int { return len([]rune(s)) }

// assertRegionOffsetsExact proves every region's RuneOffset is exact: slicing the clean text's
// runes at the offset yields the region's own text. It holds for any region whose text is a
// contiguous slice of the clean stream, which is every region the scanner captures.
func assertRegionOffsetsExact(t *testing.T, clean string, regions []speakRegion) {
	t.Helper()
	runes := []rune(clean)
	for i, r := range regions {
		end := r.RuneOffset + runesOf(r.Text)
		if r.RuneOffset < 0 || end > len(runes) {
			t.Errorf("region %d: offset %d + %d runes runs outside the clean text (%d runes)", i, r.RuneOffset, runesOf(r.Text), len(runes))
			continue
		}
		if got := string(runes[r.RuneOffset:end]); got != r.Text {
			t.Errorf("region %d: clean runes at offset %d = %q, want the region text %q", i, r.RuneOffset, got, r.Text)
		}
	}
}

// regionScan is everything one pass over a delta sequence observes.
type regionScan struct {
	clean   string
	regions []speakRegion
	found   bool
	// covered is the number of regions the closed/closedIdx reports account for (the last
	// reported closedIdx + 1) before Close; closedBeforeClose is len(Regions()) at that point.
	covered           int
	closedBeforeClose int
}

func scanRegions(deltas ...string) regionScan {
	s := newSpeakScanner()
	var b strings.Builder
	var out regionScan
	for _, d := range deltas {
		c, idx, closed := s.Write(d)
		b.WriteString(c)
		if closed {
			out.covered = idx + 1
		}
	}
	out.closedBeforeClose = len(s.Regions())
	b.WriteString(s.Close())
	out.clean = b.String()
	out.regions = s.Regions()
	out.found = s.Found()
	return out
}

// Every <speak> region is captured, in order, with its own text, lang and content offset, and
// each region's close is reported on the delta that completes its </speak> with that region's
// index. A region still open is not in Regions() until it closes.
func TestSpeakScanner_MultipleRegions(t *testing.T) {
	s := newSpeakScanner()
	steps := []struct {
		delta       string
		wantClean   string
		wantClosed  bool
		wantIdx     int
		wantRegions int // len(Regions()) after the delta
	}{
		{`<speak lang="de">Eins.</speak>`, "Eins.", true, 0, 1},
		{` Detail A <speak lang="en">Two.`, " Detail A Two.", false, 0, 1},
		{`</speak> Detail B `, " Detail B ", true, 1, 2},
		{`<speak lang="fr">Trois.</speak> Ende.`, "Trois. Ende.", true, 2, 3},
	}
	var clean strings.Builder
	for i, st := range steps {
		got, idx, closed := s.Write(st.delta)
		clean.WriteString(got)
		if got != st.wantClean {
			t.Errorf("step %d: clean = %q, want %q", i, got, st.wantClean)
		}
		if closed != st.wantClosed {
			t.Errorf("step %d: closed = %v, want %v", i, closed, st.wantClosed)
		}
		if closed && idx != st.wantIdx {
			t.Errorf("step %d: closedIdx = %d, want %d", i, idx, st.wantIdx)
		}
		if n := len(s.Regions()); n != st.wantRegions {
			t.Errorf("step %d: len(Regions()) = %d, want %d (an open region is excluded until it closes)", i, n, st.wantRegions)
		}
	}
	clean.WriteString(s.Close())
	if clean.String() != "Eins. Detail A Two. Detail B Trois. Ende." {
		t.Errorf("clean = %q, want every region's text kept and no tags", clean.String())
	}

	want := []speakRegion{
		{Text: "Eins.", Lang: "de", RuneOffset: 0},
		{Text: "Two.", Lang: "en", RuneOffset: 15},
		{Text: "Trois.", Lang: "fr", RuneOffset: 29},
	}
	if got := s.Regions(); !slices.Equal(got, want) {
		t.Errorf("Regions() = %+v, want %+v", got, want)
	}
	assertRegionOffsetsExact(t, clean.String(), s.Regions())
}

// When one delta closes several regions only one closedIdx can be returned: it is the LAST
// region that closed, and every region up to and including it is final in Regions(), so a
// caller that tracks the next index it has not handled never skips one.
func TestSpeakScanner_SeveralRegionsClosingInOneDelta(t *testing.T) {
	s := newSpeakScanner()
	clean, idx, closed := s.Write(`<speak lang="de">A</speak> x <speak lang="en">B</speak> y`)
	if clean != "A x B y" {
		t.Errorf("clean = %q, want %q", clean, "A x B y")
	}
	if !closed || idx != 1 {
		t.Errorf("closed = %v, closedIdx = %d, want true and 1 (the last region closed)", closed, idx)
	}
	want := []speakRegion{
		{Text: "A", Lang: "de", RuneOffset: 0},
		{Text: "B", Lang: "en", RuneOffset: 4},
	}
	if got := s.Regions(); !slices.Equal(got, want) {
		t.Errorf("Regions() = %+v, want %+v", got, want)
	}
}

// RuneOffset is a count of code points, not bytes, over the clean text -- including the text of
// earlier regions and the detail between them -- and a multi-byte rune cut across two deltas is
// counted once. Splitting the stream at every byte (inside a rune included) must not move it.
func TestSpeakScanner_RegionRuneOffsetsMultibyte(t *testing.T) {
	// 'ä' is two bytes, the cheese and the coffee cup are four and three: a byte offset would
	// overshoot. Region 0 starts after "Käse 🧀 " (7 runes); region 1 after that plus "Grüße"
	// (5) plus " Mitte 🧀 " (9).
	const in = `Käse 🧀 <speak lang="de">Grüße</speak> Mitte 🧀 <speak lang="en">Köln ☕</speak> Ende`
	want := []speakRegion{
		{Text: "Grüße", Lang: "de", RuneOffset: 7},
		{Text: "Köln ☕", Lang: "en", RuneOffset: 21},
	}
	const wantClean = "Käse 🧀 Grüße Mitte 🧀 Köln ☕ Ende"

	check := func(label string, deltas []string) {
		t.Helper()
		got := scanRegions(deltas...)
		if got.clean != wantClean {
			t.Errorf("%s: clean = %q, want %q", label, got.clean, wantClean)
		}
		if !slices.Equal(got.regions, want) {
			t.Errorf("%s: Regions() = %+v, want %+v", label, got.regions, want)
		}
		assertRegionOffsetsExact(t, got.clean, got.regions)
	}
	check("whole", []string{in})
	bytewise := make([]string, 0, len(in))
	for i := range len(in) {
		bytewise = append(bytewise, in[i:i+1])
	}
	check("byte-at-a-time", bytewise)
	for cut := 1; cut < len(in); cut++ {
		check("split", []string{in[:cut], in[cut:]})
	}
}

// TestSpeakScanner_RegionsChunkingIsInvisible is the chunk-invariance proof for the ordered
// capture: whole, byte-at-a-time and every two-way split agree on the clean text, every region
// (text, lang, offset), Found, and the closed reports account for exactly the regions closed
// before Close.
func TestSpeakScanner_RegionsChunkingIsInvisible(t *testing.T) {
	inputs := []string{
		`<speak lang="de">Eins.</speak> Detail <speak lang="en">Two.</speak> Ende.`,
		`Käse 🧀 <speak lang="de">Grüße</speak> Mitte 🧀 <speak lang="en">Köln ☕</speak> Ende`,
		`<SPEAK id="1"  lang = "fr" >Résumé</SPEAK ><speak lang="de">zwei</speak><speak>drei</speak>`,
		`a <speak lang="de">b <speak lang="en">c</speak> d <speak/> e <speak>f</speak> g <speak lang="x">open </spea`,
		`<speak lang="en"/>x <speak lang="de">y</speak> z`,
		`<speak></speak><speak></speak>t`,
		`<speak lang="de">one</speak> mid <speak lang="en">two </speak`,
		`no tags < here <b>x</b> <speaker>`,
	}
	for _, in := range inputs {
		want := scanRegions(in)
		check := func(label string, deltas []string) {
			t.Helper()
			got := scanRegions(deltas...)
			if got.clean != want.clean || !slices.Equal(got.regions, want.regions) || got.found != want.found {
				t.Errorf("%s on %q: got (%q, %+v, %v), want (%q, %+v, %v)", label, in,
					got.clean, got.regions, got.found, want.clean, want.regions, want.found)
			}
			if got.covered != got.closedBeforeClose {
				t.Errorf("%s on %q: closed reports cover %d regions, but %d closed before Close", label, in, got.covered, got.closedBeforeClose)
			}
			assertRegionOffsetsExact(t, got.clean, got.regions)
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

// A stream with no <speak> tag has no regions, and a lookalike tag opens none either.
func TestSpeakScanner_NoTagMeansZeroRegions(t *testing.T) {
	for _, in := range []string{"Just a plain answer.", "the <speaker> and </speakers> stay", ""} {
		s := newSpeakScanner()
		feedSpeakScanner(s, in)
		if got := s.Regions(); len(got) != 0 {
			t.Errorf("%q: Regions() = %+v, want none", in, got)
		}
		if s.Found() {
			t.Errorf("%q: Found() = true, want false", in)
		}
	}
}

// One tag is one region, and Summary()/SummaryStart() -- the phase-2 accessors -- describe
// exactly that region.
func TestSpeakScanner_OneRegionMatchesPhaseTwoAccessors(t *testing.T) {
	s := newSpeakScanner()
	feedSpeakScanner(s, `pre <speak lang="de">sum</speak> post`)
	want := []speakRegion{{Text: "sum", Lang: "de", RuneOffset: 4}}
	if got := s.Regions(); !slices.Equal(got, want) {
		t.Fatalf("Regions() = %+v, want %+v", got, want)
	}
	if text, lang := s.Summary(); text != want[0].Text || lang != want[0].Lang {
		t.Errorf("Summary() = (%q, %q), want (%q, %q)", text, lang, want[0].Text, want[0].Lang)
	}
	if got := s.SummaryStart(); got != want[0].RuneOffset {
		t.Errorf("SummaryStart() = %d, want %d", got, want[0].RuneOffset)
	}
}

// A region that is still open when the stream ends is not lost: after Close it is in Regions()
// with the text streamed so far (phase 2 spoke an unclosed first summary the same way). Until
// then it stays out, and a half-written close tag held at the end is markup, not region text.
func TestSpeakScanner_UnclosedRegionJoinsRegionsAtClose(t *testing.T) {
	s := newSpeakScanner()
	s.Write(`<speak lang="de">Eins.</speak> x <speak lang="en">hal`)
	s.Write("f </spea")
	if got := s.Regions(); len(got) != 1 || got[0].Text != "Eins." {
		t.Fatalf("Regions() before Close = %+v, want only the closed first region", got)
	}
	if tail := s.Close(); tail != "</spea" {
		t.Errorf("Close() = %q, want %q (the held bytes still reach the clean stream)", tail, "</spea")
	}
	want := []speakRegion{
		{Text: "Eins.", Lang: "de", RuneOffset: 0},
		{Text: "half ", Lang: "en", RuneOffset: 8},
	}
	if got := s.Regions(); !slices.Equal(got, want) {
		t.Errorf("Regions() after Close = %+v, want %+v", got, want)
	}
	// Close is idempotent for the regions too.
	s.Close()
	if got := s.Regions(); !slices.Equal(got, want) {
		t.Errorf("Regions() after a second Close = %+v, want %+v", got, want)
	}
}

// Nested and self-closing tags never open a region of their own: inside a region they are
// stripped like text-less markup, between regions a self-closing tag is stripped and the next
// real tag still opens its region.
func TestSpeakScanner_NestedAndSelfClosingTagsAmongRegions(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantClean string
		want      []speakRegion
	}{
		{
			"nested open is stripped, the next real tag opens a region",
			`<speak lang="de">a <speak lang="en">b</speak> c <speak lang="fr">d</speak>`,
			"a b c d",
			[]speakRegion{{Text: "a b", Lang: "de", RuneOffset: 0}, {Text: "d", Lang: "fr", RuneOffset: 6}},
		},
		{
			"self-closing before a real region does not block it",
			`<speak lang="en"/>x <speak lang="de">y</speak>`,
			"x y",
			[]speakRegion{{Text: "y", Lang: "de", RuneOffset: 2}},
		},
		{
			"self-closing between regions opens none",
			`<speak>A</speak> <speak lang="en"/> <speak>B</speak>`,
			"A  B",
			[]speakRegion{{Text: "A", Lang: "", RuneOffset: 0}, {Text: "B", Lang: "", RuneOffset: 3}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSpeakScanner()
			clean, _ := feedSpeakScanner(s, tc.in)
			if clean != tc.wantClean {
				t.Errorf("clean = %q, want %q", clean, tc.wantClean)
			}
			if got := s.Regions(); !slices.Equal(got, tc.want) {
				t.Errorf("Regions() = %+v, want %+v", got, tc.want)
			}
			if !s.Found() {
				t.Error("Found() = false, want true")
			}
			assertRegionOffsetsExact(t, clean, s.Regions())
		})
	}
}
