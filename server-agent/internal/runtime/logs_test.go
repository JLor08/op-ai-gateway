// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestLogStore builds a store with a deterministic clock so entry
// timestamps are comparable and tests never depend on wall-clock resolution.
func newTestLogStore(t *testing.T, perSpec, total int) *LogStore {
	t.Helper()
	s := NewLogStore(perSpec, total)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	var n int64
	var mu sync.Mutex
	s.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n) * time.Millisecond)
	}
	return s
}

// batchText joins every entry's text, so a test can assert on what an operator
// would actually read without caring how the writes were coalesced.
func batchText(b LogBatch) string {
	var sb strings.Builder
	for _, e := range b.Entries {
		sb.WriteString(e.Text)
	}
	return sb.String()
}

func batchesFor(batches []LogBatch, specID string) []LogBatch {
	var out []LogBatch
	for _, b := range batches {
		if b.SpecID == specID {
			out = append(out, b)
		}
	}
	return out
}

// TestLogCapacityClamping pins the operator-facing contract of the two
// settings: zero means the documented default, a too-small per-spec value is
// raised rather than obeyed, and the total can never be below one buffer (a
// combination that would otherwise resolve to "retain nothing", i.e. the
// feature silently off).
func TestLogCapacityClamping(t *testing.T) {
	cases := []struct {
		name                   string
		perSpec, total         int
		wantPerSpec, wantTotal int
	}{
		{"zero means default", 0, 0, DefaultLogBufferBytes, DefaultLogBufferTotalBytes},
		{"too small per-spec is raised", 1024, 0, minLogBufferBytes, DefaultLogBufferTotalBytes / minLogBufferBytes * minLogBufferBytes},
		{"total below per-spec keeps one buffer", 1 << 20, 4096, 1 << 20, 1 << 20},
		{"negative means default", -5, -5, DefaultLogBufferBytes, DefaultLogBufferTotalBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			perSpec, total := NewLogStore(c.perSpec, c.total).Capacity()
			if perSpec != c.wantPerSpec || total != c.wantTotal {
				t.Errorf("Capacity() = (%d, %d), want (%d, %d)", perSpec, total, c.wantPerSpec, c.wantTotal)
			}
		})
	}
}

// TestLogCollectionIsUnconditionalButStreamingIsNot is the pair of properties
// the whole design rests on, asserted together because each is only correct in
// the presence of the other: output is ALWAYS captured (so an operator who
// arrives after the incident can still read it), and NOTHING is queued for the
// wire until the gateway asks (so an unwatched fleet is silent).
func TestLogCollectionIsUnconditionalButStreamingIsNot(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(4711, ResolvedCommand{})
	p.Write([]byte("loading weights\n"))

	if got := s.Drain(); got != nil {
		t.Fatalf("Drain with nobody watching = %+v, want nil -- an unwatched fleet must produce no log traffic", got)
	}
	if tail := p.Tail(1024); !strings.Contains(tail, "loading weights") {
		t.Fatalf("Tail = %q, want the captured output: collection must not depend on a watcher", tail)
	}

	// Now someone watches: the history captured while nobody was looking is
	// exactly what they must receive first.
	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 || !batches[0].Scrollback {
		t.Fatalf("first drain after subscribe = %+v, want one scrollback batch", batches)
	}
	if text := batchText(batches[0]); !strings.Contains(text, "loading weights") {
		t.Fatalf("scrollback text = %q, want the output captured before anyone watched", text)
	}
}

// TestLogRetentionSurvivesTheProcess is the requirement change that made this
// feature useful: an operator looking at a `crashed` row wants what led to the
// crash, and the buffer used to die with the process that filled it.
func TestLogRetentionSurvivesTheProcess(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(4711, ResolvedCommand{})
	p.Write([]byte("CUDA error: out of memory\n"))
	p.Exited(1)
	// The generation is over and its procLog is gone as far as the manager is
	// concerned. Subscribing now must still find the output.

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 {
		t.Fatalf("drain = %d batches, want 1", len(batches))
	}
	if text := batchText(batches[0]); !strings.Contains(text, "CUDA error: out of memory") {
		t.Fatalf("scrollback after exit = %q, want the dead process's output", text)
	}
}

// TestLogGenerationsAppendWithBoundaryMarkers pins the crash-loop decision:
// restarts APPEND rather than replace, because the pattern across attempts is
// the diagnosis -- and each boundary is a typed marker carrying the pid and
// exit code, never synthesized text that could be confused with (or forged by)
// something the process printed.
func TestLogGenerationsAppendWithBoundaryMarkers(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)

	first := s.newProc("spec-a")
	first.Started(101, ResolvedCommand{})
	first.Write([]byte("attempt one\n"))
	first.Exited(1)

	second := s.newProc("spec-a")
	second.Started(202, ResolvedCommand{})
	second.Write([]byte("attempt two\n"))

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 {
		t.Fatalf("drain = %d batches, want 1", len(batches))
	}
	text := batchText(batches[0])
	if !strings.Contains(text, "attempt one") || !strings.Contains(text, "attempt two") {
		t.Fatalf("scrollback = %q, want BOTH generations -- a restart must append, not replace", text)
	}
	if strings.Index(text, "attempt one") > strings.Index(text, "attempt two") {
		t.Fatalf("scrollback = %q, want the older attempt first", text)
	}

	var events []LogEntry
	for _, e := range batches[0].Entries {
		if e.Event != "" {
			events = append(events, e)
		}
	}
	if len(events) != 3 {
		t.Fatalf("events = %+v, want started(101), exited(1), started(202)", events)
	}
	if events[0].Event != logEventStarted || events[0].PID != 101 {
		t.Errorf("events[0] = %+v, want started pid 101", events[0])
	}
	if events[1].Event != logEventExited || events[1].ExitCode != 1 || events[1].PID != 101 {
		t.Errorf("events[1] = %+v, want exited code 1 pid 101", events[1])
	}
	if events[2].Event != logEventStarted || events[2].PID != 202 {
		t.Errorf("events[2] = %+v, want started pid 202", events[2])
	}
	// A marker carries no text at all: nothing an agent emits can look like a
	// line the process wrote.
	for _, e := range events {
		if e.Text != "" {
			t.Errorf("event %+v carries text; markers must be structural only", e)
		}
	}
}

// TestLogScrollbackThenLive is the end-to-end shape a subscriber sees: the
// retained history exactly once, then everything written afterwards, with
// nothing duplicated across the boundary.
func TestLogScrollbackThenLive(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(1, ResolvedCommand{})
	p.Write([]byte("before-subscribe\n"))

	s.SetWatch([]string{"spec-a"})
	p.Write([]byte("after-subscribe\n"))

	batches := s.Drain()
	if len(batches) != 2 {
		t.Fatalf("drain = %d batches, want 2 (scrollback then live)", len(batches))
	}
	if !batches[0].Scrollback || batches[1].Scrollback {
		t.Fatalf("scrollback flags = %v/%v, want true then false", batches[0].Scrollback, batches[1].Scrollback)
	}
	if text := batchText(batches[0]); !strings.Contains(text, "before-subscribe") || strings.Contains(text, "after-subscribe") {
		t.Fatalf("scrollback = %q, want only what existed at subscribe time", text)
	}
	if text := batchText(batches[1]); text != "after-subscribe\n" {
		t.Fatalf("live batch = %q, want exactly the post-subscribe write (no duplication of the history)", text)
	}
}

// TestLogStdoutAndStderrShareOneStream: the requirement says both, and the two
// streams deliberately share one writer so the interleaving read back is the
// interleaving the process produced.
func TestLogStdoutAndStderrShareOneStream(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(1, ResolvedCommand{})
	s.SetWatch([]string{"spec-a"})

	// os/exec assigns the SAME writer to Stdout and Stderr (startProcess), so
	// this is literally what the two copy goroutines do.
	stdout, stderr := p, p
	stdout.Write([]byte("out-1\n"))
	stderr.Write([]byte("err-1\n"))
	stdout.Write([]byte("out-2\n"))

	batches := s.Drain()
	var text string
	for _, b := range batches {
		text += batchText(b)
	}
	if want := "out-1\nerr-1\nout-2\n"; text != want {
		t.Fatalf("combined stream = %q, want %q (both streams, in write order)", text, want)
	}
}

// TestLogEmptyScrollbackIsStillDelivered guards the honesty rule for the case
// an agent restart leaves behind: with no retained history, the subscriber
// must still receive a scrollback batch saying so. Without it, "the buffer is
// gone" is indistinguishable from "nothing has arrived yet", which is the same
// empty-window lie the feature negotiation exists to prevent.
func TestLogEmptyScrollbackIsStillDelivered(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	s.SetWatch([]string{"never-ran"})
	batches := s.Drain()
	if len(batches) != 1 {
		t.Fatalf("drain = %+v, want exactly one (empty) scrollback batch", batches)
	}
	if !batches[0].Scrollback || batches[0].SpecID != "never-ran" || len(batches[0].Entries) != 0 {
		t.Fatalf("batch = %+v, want an empty scrollback for never-ran", batches[0])
	}
	if got := s.Drain(); got != nil {
		t.Fatalf("second drain = %+v, want nil -- scrollback is one-shot", got)
	}
}

// TestLogEvictionReportsDroppedBytes: when the retention buffer overflows, the
// scrollback says exactly how much is missing. A gap that renders as silence
// would be a lie about what the process printed.
func TestLogEvictionReportsDroppedBytes(t *testing.T) {
	const cap = minLogBufferBytes
	s := newTestLogStore(t, cap, cap)
	p := s.newProc("spec-a")
	p.Started(1, ResolvedCommand{})

	chunk := strings.Repeat("a", 4096)
	const writes = 40 // 160 KiB into a 64 KiB buffer
	for range writes {
		p.Write([]byte(chunk))
	}
	p.Write([]byte("TAIL-MARKER"))

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 {
		t.Fatalf("drain = %d batches, want 1", len(batches))
	}
	text := batchText(batches[0])
	if len(text) > cap {
		t.Fatalf("retained %d bytes, want <= the configured %d", len(text), cap)
	}
	if !strings.HasSuffix(text, "TAIL-MARKER") {
		t.Fatalf("retained text does not end with the newest write")
	}
	dropped := batches[0].Entries[0].DroppedBytes
	if dropped <= 0 {
		t.Fatal("DroppedBytes = 0 after overflowing the buffer; an overflow must never look like silence")
	}
	written := int64(writes*len(chunk) + len("TAIL-MARKER"))
	// Exact, not approximate: dropped + retained must account for every byte
	// the process wrote. An off-by-something here would mean the marker is
	// misreporting the size of the gap it is announcing.
	if dropped+int64(len(text)) != written {
		t.Fatalf("dropped(%d) + retained(%d) = %d, want exactly the %d bytes written",
			dropped, len(text), dropped+int64(len(text)), written)
	}
}

// TestLogPendingOverflowReportsDroppedBytes is the same guarantee for the
// OTHER place output can be lost: a spec that outruns one flush window.
func TestLogPendingOverflowReportsDroppedBytes(t *testing.T) {
	s := newTestLogStore(t, 1<<20, 1<<20)
	p := s.newProc("spec-a")
	p.Started(1, ResolvedCommand{})
	s.SetWatch([]string{"spec-a"})
	s.Drain() // consume the (empty) scrollback

	chunk := strings.Repeat("b", 32<<10)
	const writes = 16 // 512 KiB into a 64 KiB live queue
	for range writes {
		p.Write([]byte(chunk))
	}

	batches := s.Drain()
	if len(batches) != 1 {
		t.Fatalf("drain = %d batches, want 1", len(batches))
	}
	dropped := batches[0].Entries[0].DroppedBytes
	if dropped <= 0 {
		t.Fatal("DroppedBytes = 0 after overflowing the live queue; the gap must be visible")
	}
	if got := int64(len(batchText(batches[0]))) + dropped; got != int64(writes*len(chunk)) {
		t.Fatalf("delivered(%d) + dropped(%d) != written(%d)", len(batchText(batches[0])), dropped, writes*len(chunk))
	}
	// The retained history is unaffected: the live queue overflowing is a rate
	// problem, not a history problem, and an operator can still scroll back.
	if tail := p.Tail(1 << 20); len(tail) != writes*len(chunk) {
		t.Fatalf("retained %d bytes after a live-queue overflow, want all %d -- retention and streaming must not share a fate", len(tail), writes*len(chunk))
	}
}

// TestLogTotalCeilingEvictsSpecBuffers pins the bound the coordinator asked
// for: the per-spec capacity is not the memory number, the TOTAL is, and it is
// enforced by dropping whole spec buffers -- least-recently-written first, and
// never one someone is watching while an unwatched one is available.
func TestLogTotalCeilingEvictsSpecBuffers(t *testing.T) {
	const per = minLogBufferBytes
	s := newTestLogStore(t, per, per*2) // room for exactly two spec buffers

	for _, id := range []string{"old", "mid"} {
		p := s.newProc(id)
		p.Started(1, ResolvedCommand{})
		p.Write([]byte(id + "-output"))
	}
	// A third spec must push the least-recently-written one out.
	third := s.newProc("new")
	third.Started(1, ResolvedCommand{})
	third.Write([]byte("new-output"))

	s.SetWatch([]string{"old", "mid", "new"})
	batches := s.Drain()
	got := map[string]string{}
	for _, b := range batches {
		got[b.SpecID] = batchText(b)
	}
	if got["old"] != "" {
		t.Errorf("spec %q survived the total ceiling with %q; the oldest buffer must be evicted", "old", got["old"])
	}
	if !strings.Contains(got["mid"], "mid-output") || !strings.Contains(got["new"], "new-output") {
		t.Errorf("retained buffers = %+v, want the two most recent", got)
	}
	if _, perTotal := s.Capacity(); perTotal != per*2 {
		t.Errorf("Capacity total = %d, want %d", perTotal, per*2)
	}
}

// TestLogRetainDropsRemovedSpecs: retention follows the SPEC, so deleting the
// spec releases its buffer -- there is no row left to open a log view on.
func TestLogRetainDropsRemovedSpecs(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes*4)
	for _, id := range []string{"keep", "drop"} {
		p := s.newProc(id)
		p.Started(1, ResolvedCommand{})
		p.Write([]byte(id + "-output"))
	}
	s.Retain([]string{"keep"})

	s.SetWatch([]string{"keep", "drop"})
	got := map[string]string{}
	for _, b := range s.Drain() {
		got[b.SpecID] = batchText(b)
	}
	if !strings.Contains(got["keep"], "keep-output") {
		t.Errorf("kept spec lost its history: %q", got["keep"])
	}
	if got["drop"] != "" {
		t.Errorf("removed spec kept %q; a deleted spec must release its buffer", got["drop"])
	}
}

// TestLogUnwatchStopsQueueing: the last viewer leaving must stop the flow
// AND release whatever was queued for them -- memory held for nobody.
func TestLogUnwatchStopsQueueing(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(1, ResolvedCommand{})
	s.SetWatch([]string{"spec-a"})
	p.Write([]byte("watched\n"))

	s.SetWatch(nil) // the last viewer closed the log view
	p.Write([]byte("unwatched\n"))

	if got := s.Drain(); got != nil {
		t.Fatalf("drain after unwatch = %+v, want nil", got)
	}
	if w := s.Watching(); len(w) != 0 {
		t.Fatalf("Watching() = %v, want empty", w)
	}
	// Re-subscribing replays from the retention buffer, which kept both.
	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 || !batches[0].Scrollback {
		t.Fatalf("re-subscribe = %+v, want a fresh scrollback", batches)
	}
	if text := batchText(batches[0]); !strings.Contains(text, "watched") || !strings.Contains(text, "unwatched") {
		t.Fatalf("scrollback = %q, want everything captured while unwatched too", text)
	}
}

// TestLogWatchIsFullSetNotDelta pins the command contract: each SetWatch
// states the whole desired set, so adding one spec does not silently keep an
// old one streaming.
func TestLogWatchIsFullSetNotDelta(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes*4)
	s.SetWatch([]string{"a", "b"})
	if got := s.Watching(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("Watching() = %v, want [a b]", got)
	}
	s.SetWatch([]string{"b", "c"})
	if got := s.Watching(); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("Watching() = %v, want [b c] -- the set is replaced, never merged", got)
	}
	// "b" was already watched, so it must NOT get a second scrollback replay.
	s.Drain() // first scrollbacks for a, b
	pa := s.newProc("b")
	pa.Started(1, ResolvedCommand{})
	pa.Write([]byte("live"))
	for _, b := range batchesFor(s.Drain(), "b") {
		if b.Scrollback {
			t.Fatal("a spec that stayed in the watch set was replayed again")
		}
	}
}

// TestLogDrainBudgetIsFairAcrossSpecs: the per-drain byte budget must not
// systematically starve whichever spec sorts last, or one chatty model would
// permanently hide another's output from the operator.
func TestLogDrainBudgetIsFairAcrossSpecs(t *testing.T) {
	s := newTestLogStore(t, 1<<20, 8<<20)
	ids := []string{"aaa", "bbb", "ccc"}
	procs := map[string]*procLog{}
	for _, id := range ids {
		p := s.newProc(id)
		p.Started(1, ResolvedCommand{})
		procs[id] = p
	}
	s.SetWatch(ids)
	s.Drain() // consume the empty scrollbacks

	// Three specs each queue more than a third of the per-drain budget, so no
	// single drain can serve all of them -- whoever is deferred must be picked
	// up by a later round rather than being permanently hidden behind the
	// spec that happens to sort first.
	for _, id := range ids {
		procs[id].Write([]byte(strings.Repeat("x", 48<<10)))
	}
	seen := map[string]bool{}
	drains := 0
	for len(seen) < len(ids) && drains < 10 {
		drains++
		for _, b := range s.Drain() {
			if len(b.Entries) > 0 {
				seen[b.SpecID] = true
			}
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("spec %q was never drained in %d rounds; the budget must rotate", id, drains)
		}
	}
	if drains < 2 {
		t.Fatalf("all three specs drained in %d round(s); the test no longer exercises the budget", drains)
	}
}

// TestLogTailIsGenerationScoped: LastError.StderrTail describes ONE failed
// run, and the buffer now spans all of them -- so the tail must not blend a
// previous attempt's output into this attempt's error.
func TestLogTailIsGenerationScoped(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	first := s.newProc("spec-a")
	first.Started(1, ResolvedCommand{})
	first.Write([]byte("FIRST-GENERATION\n"))
	first.Exited(1)

	second := s.newProc("spec-a")
	second.Started(2, ResolvedCommand{})
	second.Write([]byte("SECOND-GENERATION\n"))

	if tail := second.Tail(4096); strings.Contains(tail, "FIRST-GENERATION") {
		t.Errorf("second generation's Tail = %q, want only its own output", tail)
	}
	if tail := second.Tail(4096); !strings.Contains(tail, "SECOND-GENERATION") {
		t.Errorf("second generation's Tail = %q, want its own output", tail)
	}
	if tail := first.Tail(4096); !strings.Contains(tail, "FIRST-GENERATION") || strings.Contains(tail, "SECOND-GENERATION") {
		t.Errorf("first generation's Tail = %q, want only its own output", tail)
	}
	if got := second.Tail(0); got != "" {
		t.Errorf("Tail(0) = %q, want empty", got)
	}
}

// TestLogConcurrentWritersAreRaceSafe reproduces what os/exec actually does:
// two copying goroutines per process, several processes at once, while a
// consumer drains and the watch set changes underneath. Meaningful under
// -race, which is how this package is verified.
func TestLogConcurrentWritersAreRaceSafe(t *testing.T) {
	s := NewLogStore(minLogBufferBytes, minLogBufferBytes*4)
	// Two waitgroups, not one: the drainer below exits on `stop`, which is
	// closed only after the finite producers are done, so putting it in the
	// same group as them would be a deadlock rather than a race test.
	var producers, helpers sync.WaitGroup
	stop := make(chan struct{})

	for g := range 4 {
		p := s.newProc("spec-" + string(rune('a'+g)))
		p.Started(1000+g, ResolvedCommand{})
		for range 2 { // stdout and stderr, exactly as os/exec drives them
			producers.Add(1)
			go func() {
				defer producers.Done()
				for range 400 {
					p.Write([]byte("chunk"))
				}
			}()
		}
	}
	producers.Add(1)
	go func() {
		defer producers.Done()
		for i := range 200 {
			if i%2 == 0 {
				s.SetWatch([]string{"spec-a", "spec-c"})
			} else {
				s.SetWatch(nil)
			}
		}
	}()
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.Drain()
				s.Watching()
			}
		}
	}()

	producers.Wait()
	close(stop)
	helpers.Wait()
}

// TestLogEntriesFromReportsALossWithNoRecords: a drop with nothing to attach
// it to still has to be reported. Swallowing it would turn a gap back into
// silence in the one case where there is nothing else to see.
func TestLogEntriesFromReportsALossWithNoRecords(t *testing.T) {
	entries := entriesFrom(nil, 512)
	if len(entries) != 1 || entries[0].DroppedBytes != 512 || entries[0].Text != "" {
		t.Fatalf("entriesFrom(nil, 512) = %+v, want one entry carrying only the loss", entries)
	}
	if got := entriesFrom(nil, 0); len(got) != 0 {
		t.Fatalf("entriesFrom(nil, 0) = %+v, want no entries", got)
	}
}

// TestLogNilStoreIsSafe: every method must tolerate a nil receiver, matching
// the nil-safety discipline this module applies to every registry-shaped type.
func TestLogNilStoreIsSafe(t *testing.T) {
	var s *LogStore
	s.SetWatch([]string{"a"})
	s.Retain([]string{"a"})
	if got := s.Drain(); got != nil {
		t.Errorf("nil Drain = %+v", got)
	}
	if got := s.Watching(); got != nil {
		t.Errorf("nil Watching = %+v", got)
	}
	if per, total := s.Capacity(); per != 0 || total != 0 {
		t.Errorf("nil Capacity = (%d,%d)", per, total)
	}
	p := s.newProc("a")
	p.Started(1, ResolvedCommand{})
	if n, err := p.Write([]byte("x")); n != 1 || err != nil {
		t.Errorf("nil-store Write = (%d, %v), want (1, nil)", n, err)
	}
	p.Exited(0)
	if got := p.Tail(10); got != "" {
		t.Errorf("nil-store Tail = %q", got)
	}
}

// --- the resolved launch command ------------------------------------------

// testCommand builds a ResolvedCommand distinguishable per generation, so a
// test can tell WHICH generation's command a marker carried.
func testCommand(binary string, port string) ResolvedCommand {
	return ResolvedCommand{Binary: binary, Args: []string{"--port", port}, Env: []string{"PATH=/bin"}}
}

// commandsOf returns the (entry index, command) pairs in a snapshot, so a test
// can assert both WHAT was reported and WHERE.
func commandsOf(b LogBatch) []LogEntry {
	var out []LogEntry
	for _, e := range b.Entries {
		if e.Command != nil {
			out = append(out, e)
		}
	}
	return out
}

// TestLogCommandRidesTheStartedMarker is the placement decision, pinned: the
// resolved command is a typed field on the generation's OPENING MARKER, not a
// per-spec value and not text in the stream.
//
// Both halves matter. On the marker, because the marker is the generation
// boundary and already carries the pid, so "this pid, running this command" is
// one record and cannot drift onto another attempt. TYPED, because synthesized
// text is indistinguishable from output the process printed -- and therefore
// forgeable by a model server printing a convincing marker line.
func TestLogCommandRidesTheStartedMarker(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(4711, testCommand("/opt/llama/llama-server", "54331"))
	p.Write([]byte("loading weights\n"))

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 || !batches[0].Scrollback {
		t.Fatalf("drain = %+v, want one scrollback batch", batches)
	}
	carriers := commandsOf(batches[0])
	if len(carriers) != 1 {
		t.Fatalf("%d entries carry a command, want exactly 1 (the opening marker)", len(carriers))
	}
	if carriers[0].Event != logEventStarted {
		t.Errorf("the command is attached to a %q entry, want the opening marker", carriers[0].Event)
	}
	if carriers[0].PID != 4711 {
		t.Errorf("marker pid = %d, want 4711 -- the pid it is keyed to lives on the same record", carriers[0].PID)
	}
	if carriers[0].Command.Binary != "/opt/llama/llama-server" || strings.Join(carriers[0].Command.Args, " ") != "--port 54331" {
		t.Errorf("command = %+v, want the recorded binary and argv", carriers[0].Command)
	}
	// Structure only: a marker never carries text, so nothing the portal
	// renders as a marker can be forged by the process.
	if carriers[0].Text != "" {
		t.Errorf("the marker carries text %q; the command must be a typed field, never text in the stream", carriers[0].Text)
	}
	// And no other entry carries one: the command describes one generation.
	for _, e := range batches[0].Entries {
		if e.Event == "" && e.Command != nil {
			t.Errorf("an output entry carries a command: %+v", e)
		}
	}
}

// TestLogCommandTravelsWithEachGenerationsOwnMarker is what the marker
// placement buys, and it is strictly more than a single "latest command" could
// show: across a crash loop EVERY attempt's own command sits with its own
// output, so the operator can see that ${PORT} differed between attempts --
// which is exactly the kind of difference a one-command view makes invisible.
func TestLogCommandTravelsWithEachGenerationsOwnMarker(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)

	for i, port := range []string{"40001", "40002", "40003"} {
		p := s.newProc("spec-a")
		p.Started(101+i, testCommand("/opt/a", port))
		p.Write([]byte("attempt " + port + "\n"))
		if i < 2 {
			p.Exited(1)
		}
	}

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 || !batches[0].Scrollback {
		t.Fatalf("drain = %+v, want one scrollback batch", batches)
	}
	carriers := commandsOf(batches[0])
	if len(carriers) != 3 {
		t.Fatalf("%d entries carry a command, want one per generation (3)", len(carriers))
	}
	wantPorts := []string{"40001", "40002", "40003"}
	wantPIDs := []int{101, 102, 103}
	for i, e := range carriers {
		if e.PID != wantPIDs[i] {
			t.Errorf("carrier %d pid = %d, want %d", i, e.PID, wantPIDs[i])
		}
		if got := strings.Join(e.Command.Args, " "); got != "--port "+wantPorts[i] {
			t.Errorf("carrier %d args = %q, want the port THAT attempt resolved (%s)", i, got, wantPorts[i])
		}
	}
	// Each command precedes its own generation's output, in order.
	text := batchText(batches[0])
	if strings.Index(text, "attempt 40001") > strings.Index(text, "attempt 40003") {
		t.Fatalf("generations are out of order: %q", text)
	}
}

// TestLogStartFailedMarkerCarriesTheCommandAndNoPID: an exec that failed
// produced no process and no output, so the command is the ENTIRE content of the
// log view -- the case an operator opens it for most often. It gets its own
// event kind rather than a pid-0 "started", which would claim that output begins
// there.
func TestLogStartFailedMarkerCarriesTheCommandAndNoPID(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.StartFailed(testCommand("/opt/does-not-exist", "54331"))

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 || !batches[0].Scrollback {
		t.Fatalf("drain = %+v, want one scrollback batch", batches)
	}
	if len(batches[0].Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly the failed-start marker", batches[0].Entries)
	}
	e := batches[0].Entries[0]
	if e.Event != logEventStartFailed {
		t.Errorf("event = %q, want %q -- never a pid-0 %q, which would claim output begins here", e.Event, logEventStartFailed, logEventStarted)
	}
	if e.PID != 0 {
		t.Errorf("pid = %d, want 0: no process ever existed", e.PID)
	}
	if e.Command == nil || e.Command.Binary != "/opt/does-not-exist" {
		t.Fatalf("command = %+v, want the binary the failed exec attempted", e.Command)
	}
}

// TestLogCommandIsEvictedWithItsMarker is the ACCEPTED COST of putting the
// command on a record inside the bounded ring, asserted rather than assumed so
// that it stays a decision.
//
// The ring trims from the front, so a generation that prints more than the
// per-spec capacity loses its own opening marker -- and with it the only copy of
// its command. What must never happen is that this reads as "there was no
// command": the retained history then begins with OUTPUT rather than with a
// marker, which is precisely the condition the portal turns into a stated
// notice, exactly like a dropped-bytes gap.
func TestLogCommandIsEvictedWithItsMarker(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(4711, testCommand("/opt/llama/llama-server", "54331"))
	line := strings.Repeat("y", 4096) + "\n"
	for range (minLogBufferBytes / len(line)) * 3 {
		p.Write([]byte(line))
	}

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 || !batches[0].Scrollback {
		t.Fatalf("drain = %+v, want one scrollback batch", batches)
	}
	if hasEvent(batches[0], logEventStarted) {
		t.Fatal("the opening marker was NOT evicted, so this test is not exercising what it claims")
	}
	if len(commandsOf(batches[0])) != 0 {
		t.Fatal("a command survived its own marker's eviction; nothing outside the ring retains one")
	}
	// The detectable condition: the history begins with output, so the reader
	// knows a generation's opening is missing rather than assuming there was
	// none. The dropped-byte count says the same thing about the text.
	if batches[0].Entries[0].Event != "" || batches[0].Entries[0].Text == "" {
		t.Fatalf("first entry = %+v, want output -- that is what tells a reader the opening marker is gone", batches[0].Entries[0])
	}
	if batches[0].Entries[0].DroppedBytes == 0 {
		t.Error("the evicted output was not reported as a gap")
	}
}

// TestLogCommandIsChargedAgainstTheBufferCapacity: a marker is no longer
// necessarily tiny, so the per-spec capacity has to count what it carries.
// Otherwise a crash loop of command-bearing markers would grow the buffer past
// the operator's setting -- silently, since none of it is text.
func TestLogCommandIsChargedAgainstTheBufferCapacity(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	big := ResolvedCommand{Binary: "/opt/a", Args: []string{strings.Repeat("x", 8<<10)}, Env: []string{strings.Repeat("y", 8<<10)}}
	for i := range 40 {
		p := s.newProc("spec-a")
		p.Started(1000+i, big)
		p.Exited(1)
	}

	s.mu.Lock()
	got := s.logs["spec-a"].bytes
	s.mu.Unlock()
	if got > s.perSpec {
		t.Fatalf("retained bytes = %d, want at most the per-spec capacity %d -- a command must be charged like text", got, s.perSpec)
	}
}

// TestLogCommandIsReleasedWithTheSpec: a spec removed from the desired config
// has no row left to open a log view on, so its commands go with its output --
// released by the same call, not left behind as a leak the output tests could
// not see.
func TestLogCommandIsReleasedWithTheSpec(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, minLogBufferBytes)
	p := s.newProc("spec-a")
	p.Started(4711, testCommand("/opt/a", "1"))
	p.Write([]byte("hello\n"))

	s.Retain([]string{"other-spec"})

	s.SetWatch([]string{"spec-a"})
	batches := s.Drain()
	if len(batches) != 1 {
		t.Fatalf("drain = %+v, want the (now empty) scrollback", batches)
	}
	if len(batches[0].Entries) != 0 {
		t.Fatalf("a removed spec kept entries: %+v", batches[0].Entries)
	}
}
