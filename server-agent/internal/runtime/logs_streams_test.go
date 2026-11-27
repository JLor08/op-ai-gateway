// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// streamText joins the text of every entry on stream, across batches, in order.
func streamText(batches []LogBatch, stream string) string {
	var sb strings.Builder
	for _, b := range batches {
		for _, e := range b.Entries {
			if e.Stream == stream {
				sb.WriteString(e.Text)
			}
		}
	}
	return sb.String()
}

// streamDropped sums dropped_bytes over every entry on stream.
func streamDropped(batches []LogBatch, stream string) int64 {
	var n int64
	for _, b := range batches {
		for _, e := range b.Entries {
			if e.Stream == stream {
				n += e.DroppedBytes
			}
		}
	}
	return n
}

// ringBytesTotal recomputes what the store's byte accounting should say, so a
// test can check the incremental counter against the truth. Caller holds s.mu.
func ringBytesTotal(s *LogStore) int {
	n := 0
	for _, l := range s.logs {
		for _, r := range l.streams {
			n += r.bytes
		}
	}
	return n
}

// TestStreamRingsDoNotEvictEachOther: a log file that floods its stream must
// not push the process output out of the spec's history. Each stream has its
// own ring, trimmed against its own cap, and each reports its own gap.
func TestStreamRingsDoNotEvictEachOther(t *testing.T) {
	s := newTestLogStore(t, 64<<10, 0)
	p := s.newProc("S")
	p.Started(1, ResolvedCommand{})
	// Process output before the flood too: only a ring of its own keeps it --
	// in a shared ring the flood would evict it, and a check of the output
	// written AFTER the flood alone could not tell the difference.
	early := "early process line\n"
	p.Write([]byte(early))
	engine := p.ForStream("engine")
	chunk := strings.Repeat("e", 4096)
	for range (1 << 20) / len(chunk) {
		engine.Write([]byte(chunk))
	}
	proc := strings.Repeat("p", 1023) + "\n"
	p.Write([]byte(proc))

	s.SetWatch([]string{"S"}, nil)
	batches := drainAll(t, s)
	if got := streamText(batches, ""); got != early+proc {
		t.Fatalf("process text = %d bytes, want the %d bytes written, intact", len(got), len(early+proc))
	}
	if d := streamDropped(batches, ""); d != 0 {
		t.Fatalf("process stream reports %d dropped bytes; a file flood must not evict it", d)
	}
	gap := false
	for _, b := range batches {
		for _, e := range b.Entries {
			if e.DroppedBytes > 0 {
				if e.Stream != "engine" {
					t.Fatalf("a gap entry carries stream %q, want %q", e.Stream, "engine")
				}
				gap = true
			}
		}
	}
	if !gap {
		t.Fatal("the flooded engine stream reports no dropped_bytes; its overflow must be visible")
	}
	retained := int64(len(streamText(batches, "engine")))
	if retained+streamDropped(batches, "engine") != 1<<20 {
		t.Fatalf("engine retained(%d) + dropped(%d) != written(%d)", retained, streamDropped(batches, "engine"), 1<<20)
	}
}

// TestTotalIsAByteBoundOverStreams: the total ceiling counts the bytes of every
// stream of every spec, holds after every single write, and is restored by
// dropping whole unwatched specs, oldest first -- never a watched one.
func TestTotalIsAByteBoundOverStreams(t *testing.T) {
	const per = 64 << 10
	fill := func(t *testing.T, s *LogStore, id string) {
		t.Helper()
		p := s.newProc(id)
		chunk := strings.Repeat("x", 4096)
		for _, w := range []*procLog{p, p.ForStream("engine")} {
			for range 2 * per / len(chunk) {
				w.Write([]byte(chunk))
				s.mu.Lock()
				used, total, truth := s.used, s.total, ringBytesTotal(s)
				s.mu.Unlock()
				if used > total {
					t.Fatalf("used = %d after a write, over the total %d", used, total)
				}
				if used != truth {
					t.Fatalf("used = %d, but the rings hold %d", used, truth)
				}
			}
		}
	}
	t.Run("oldest unwatched spec is evicted", func(t *testing.T) {
		// 256 KiB is raised to per + 4*64 KiB = 320 KiB (TestTotalRaisedToFitFileRings):
		// room for two specs with two full streams each, not three.
		s := newTestLogStore(t, per, 256<<10)
		if _, total := s.Capacity(); total != per+4*minLogBufferBytes {
			t.Fatalf("Capacity total = %d, want %d", total, per+4*minLogBufferBytes)
		}
		for _, id := range []string{"s0", "s1", "s2"} {
			fill(t, s, id)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.logs["s0"] != nil {
			t.Error("the oldest spec survived the total ceiling")
		}
		if s.logs["s1"] == nil || s.logs["s2"] == nil {
			t.Error("a newer spec was evicted instead of the oldest")
		}
	})
	t.Run("a watched spec is never the victim", func(t *testing.T) {
		s := newTestLogStore(t, per, 256<<10)
		s.SetWatch([]string{"s0"}, nil)
		for _, id := range []string{"s0", "s1", "s2"} {
			fill(t, s, id)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.logs["s0"] == nil {
			t.Error("the watched spec was evicted")
		}
		if s.logs["s1"] != nil {
			t.Error("the oldest UNWATCHED spec survived the total ceiling")
		}
	})
}

// TestTotalHoldsWhenOnlyWatchedSpecsRemain: when every other spec is watched
// (or there is no other spec), nothing can be evicted, and the total still
// holds: the ring just written gives up its own oldest bytes, counted as a gap.
func TestTotalHoldsWhenOnlyWatchedSpecsRemain(t *testing.T) {
	const per = 64 << 10
	// Raised to per + 4*64 KiB: exactly one spec with five full rings.
	s := newTestLogStore(t, per, per)
	s.SetWatch([]string{"w"}, nil)
	w := s.newProc("w")
	chunk := strings.Repeat("w", 4096)
	for _, sw := range []*procLog{w, w.ForStream("a"), w.ForStream("b"), w.ForStream("c"), w.ForStream("d")} {
		for range 2 * per / len(chunk) {
			sw.Write([]byte(chunk))
		}
	}
	late := s.newProc("late")
	late.Write([]byte("late-output"))

	s.mu.Lock()
	used, total, truth := s.used, s.total, ringBytesTotal(s)
	watchedKept := s.logs["w"] != nil
	s.mu.Unlock()
	if used > total || used != truth {
		t.Fatalf("used = %d (rings %d), want at most the total %d", used, truth, total)
	}
	if !watchedKept {
		t.Fatal("the watched spec was evicted")
	}
	s.SetWatch([]string{"w", "late"}, nil)
	batches := batchesFor(drainAll(t, s), "late")
	got := int64(len(streamText(batches, ""))) + streamDropped(batches, "")
	if got != int64(len("late-output")) {
		t.Fatalf("late spec retained+dropped = %d, want the %d bytes written -- a squeezed ring must count its gap", got, len("late-output"))
	}
}

// TestTotalRaisedToFitFileRings: a total too small for one spec's process ring
// plus four file rings at the floor is raised to fit them, and Capacity reports
// the effective number for the startup log line.
func TestTotalRaisedToFitFileRings(t *testing.T) {
	s := NewLogStore(1<<20, 1<<20)
	if per, total := s.Capacity(); per != 1<<20 || total != 1<<20+4*(64<<10) {
		t.Fatalf("Capacity() = (%d, %d), want (%d, %d)", per, total, 1<<20, 1<<20+4*(64<<10))
	}
	if s.fileCap != minLogBufferBytes {
		t.Fatalf("file ring cap = %d, want the %d floor", s.fileCap, minLogBufferBytes)
	}
	if d := NewLogStore(0, 0); d.fileCap != DefaultLogBufferBytes {
		t.Fatalf("default file ring cap = %d, want the per-spec %d", d.fileCap, DefaultLogBufferBytes)
	}
	// Clamped equally to the remainder: (4 MiB - 1 MiB) / 4.
	if c := NewLogStore(1<<20, 4<<20); c.fileCap != (3<<20)/4 {
		t.Fatalf("file ring cap = %d, want %d", c.fileCap, (3<<20)/4)
	}
}

// TestTailIgnoresFileStreams: StderrTail is process output only. File text of
// the same generation must not leak into it.
func TestTailIgnoresFileStreams(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	p := s.newProc("S")
	p.Started(1, ResolvedCommand{})
	p.Write([]byte("PROCESS\n"))
	f := p.ForStream("engine")
	f.Mark(logEventFileOpened)
	f.Write([]byte("FILE\n"))

	if got := s.tail("S", p.gen, 2048); got != "PROCESS\n" {
		t.Fatalf("tail = %q, want the process text only", got)
	}
	if got := p.Tail(2048); got != "PROCESS\n" {
		t.Fatalf("procLog.Tail = %q, want the process text only", got)
	}
	if got := s.tailStream("S", "engine", p.gen, 2048); got != "FILE\n" {
		t.Fatalf("tailStream(engine) = %q, want the file text", got)
	}
	if got := s.tailStream("S", "missing", p.gen, 2048); got != "" {
		t.Fatalf("tailStream(missing) = %q, want empty", got)
	}
}

// TestDrainIsPerStreamUnit: each (spec, stream) has its own live queue, so a
// file at its rate cap cannot cause drops in the live process view, and one
// Drain serves both.
func TestDrainIsPerStreamUnit(t *testing.T) {
	s := newTestLogStore(t, 1<<20, 0)
	p := s.newProc("S")
	p.Started(1, ResolvedCommand{})
	s.SetWatch([]string{"S"}, nil)
	s.Drain() // the (marker-only) scrollback

	engine := p.ForStream("engine")
	chunk := strings.Repeat("e", 4096)
	for range 20 { // 80 KiB into a 64 KiB queue
		engine.Write([]byte(chunk))
	}
	p.Write([]byte("process-line\n"))

	batches := s.Drain()
	var proc, eng *LogBatch
	for i := range batches {
		b := &batches[i]
		if b.Scrollback {
			t.Fatalf("live drain produced a scrollback batch: %+v", b)
		}
		if len(b.Entries) == 0 {
			t.Fatalf("empty live batch")
		}
		stream := b.Entries[0].Stream
		for _, e := range b.Entries {
			if e.Stream != stream {
				t.Fatalf("one batch mixes streams %q and %q; a live batch is one (spec, stream) unit", stream, e.Stream)
			}
		}
		switch stream {
		case "":
			proc = b
		case "engine":
			eng = b
		}
	}
	if proc == nil || eng == nil {
		t.Fatalf("one drain = %d batches, want a process batch and an engine batch", len(batches))
	}
	if batchText(*proc) != "process-line\n" || streamDropped(batches, "") != 0 {
		t.Fatalf("process batch = %q (dropped %d), want its line and no drop", batchText(*proc), streamDropped(batches, ""))
	}
	if d := streamDropped(batches, "engine"); d <= 0 || int64(len(batchText(*eng)))+d != 20*4096 {
		t.Fatalf("engine delivered %d + dropped %d, want %d with a visible gap", len(batchText(*eng)), d, 20*4096)
	}
}

// TestReplayCoversAllStreamsInOrder: a replay is ONE chunk sequence for the
// spec -- the process stream first, then each file stream by name -- with every
// entry naming its stream. It stays one sequence when it spans several frames:
// every chunk is flagged Scrollback (the gateway's routing key) and every one
// but the last More, so no stream boundary can look like the end of the replay.
func TestReplayCoversAllStreamsInOrder(t *testing.T) {
	s := newTestLogStore(t, 1<<20, 0)
	p := s.newProc("S")
	p.Started(1, ResolvedCommand{})
	line := strings.Repeat("l", 99) + "\n"
	writers := []struct {
		stream string
		w      *procLog
	}{{"engine", p.ForStream("engine")}, {"", p}, {"alpha", p.ForStream("alpha")}}
	for range (1 << 20) / len(line) {
		for _, w := range writers {
			w.w.Write([]byte(line))
		}
	}

	s.SetWatch([]string{"S"}, nil)
	batches := drainAll(t, s)
	if len(batches) < 3 {
		t.Fatalf("replay = %d chunks, want several (the test needs a multi-frame replay)", len(batches))
	}
	order := []string{}
	for i, b := range batches {
		if !b.Scrollback {
			t.Fatalf("chunk %d is not flagged scrollback", i)
		}
		if last := i == len(batches)-1; b.More == last {
			t.Fatalf("chunk %d of %d has More=%v; one sequence flags More on all but the last", i, len(batches), b.More)
		}
		for _, e := range b.Entries {
			if len(order) == 0 || order[len(order)-1] != e.Stream {
				order = append(order, e.Stream)
			}
		}
	}
	if strings.Join(order, ",") != ",alpha,engine" {
		t.Fatalf("stream order in the replay = %q, want process, alpha, engine", order)
	}
	for _, st := range []string{"", "alpha", "engine"} {
		got := int64(len(streamText(batches, st))) + streamDropped(batches, st)
		if got != int64((1<<20)/len(line)*len(line)) {
			t.Errorf("stream %q: retained+dropped = %d, want every byte written", st, got)
		}
	}
}

// TestCoalesceKeepsStreamsApart: coalescing joins consecutive writes of one
// stream and generation only. Interleaved writes of two streams of the same
// generation must come out as two separate, intact histories, in the ring and
// in the live queue alike.
func TestCoalesceKeepsStreamsApart(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	p := s.newProc("S")
	f := p.ForStream("engine")
	s.SetWatch([]string{"S"}, nil)
	s.Drain()
	for i := range 4 {
		p.Write([]byte("P" + itoa(i)))
		f.Write([]byte("E" + itoa(i)))
	}

	live := s.Drain()
	if streamText(live, "") != "P0P1P2P3" || streamText(live, "engine") != "E0E1E2E3" {
		t.Fatalf("live: process %q, engine %q", streamText(live, ""), streamText(live, "engine"))
	}
	s.mu.Lock()
	for name, r := range s.logs["S"].streams {
		for _, rec := range r.recs {
			if rec.stream != name {
				t.Errorf("ring %q holds a record of stream %q", name, rec.stream)
			}
		}
		if len(r.recs) != 1 {
			t.Errorf("ring %q holds %d records, want its writes coalesced into one", name, len(r.recs))
		}
	}
	s.mu.Unlock()

	s.SetWatch([]string{"S"}, map[string]uint64{"S": 1}) // a second viewer arrives
	replay := drainAll(t, s)
	if streamText(replay, "") != "P0P1P2P3" || streamText(replay, "engine") != "E0E1E2E3" {
		t.Fatalf("replay: process %q, engine %q", streamText(replay, ""), streamText(replay, "engine"))
	}
}

// TestGapIsCountedOnItsStreamWhereItHappened: bytes a tailer skips (its read
// rate cap) are a gap on that stream only -- in the retained history at the
// position they were skipped, and in the live queue of that stream's unit.
func TestGapIsCountedOnItsStreamWhereItHappened(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	p := s.newProc("S")
	f := p.ForStream("engine")
	f.Write([]byte("before\n"))
	s.gap(f, 1000)
	s.gap(f, 24)
	f.Write([]byte("after\n"))
	p.Write([]byte("process\n"))

	s.SetWatch([]string{"S"}, nil)
	replay := drainAll(t, s)
	var eng []LogEntry
	for _, b := range replay {
		for _, e := range b.Entries {
			if e.Stream == "engine" {
				eng = append(eng, e)
			}
		}
	}
	if len(eng) != 2 || eng[0].Text != "before\n" || eng[0].DroppedBytes != 0 ||
		eng[1].Text != "after\n" || eng[1].DroppedBytes != 1024 {
		t.Fatalf("engine replay = %+v, want before, then after carrying the 1024-byte gap", eng)
	}
	if streamDropped(replay, "") != 0 {
		t.Fatal("a file gap leaked onto the process stream")
	}

	s.gap(f, 7)
	live := s.Drain()
	if len(live) != 1 || live[0].Entries[0].Stream != "engine" || live[0].Entries[0].DroppedBytes != 7 {
		t.Fatalf("live = %+v, want one gap-only engine entry of 7 bytes", live)
	}
}

// TestMarkIsATypedEntryOnItsStream: a file marker is a typed event on the file's
// stream, with no pid and no command, live and in the replay.
func TestMarkIsATypedEntryOnItsStream(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	p := s.newInstanceProc("S", 2)
	p.Started(77, ResolvedCommand{})
	f := p.ForStream("engine")
	s.SetWatch([]string{"S"}, nil)
	s.Drain()
	f.Mark(logEventFileOpened)
	check := func(where string, batches []LogBatch) {
		t.Helper()
		var found bool
		for _, b := range batches {
			for _, e := range b.Entries {
				if e.Event != logEventFileOpened {
					continue
				}
				found = true
				if e.Stream != "engine" || e.PID != 0 || e.Command != nil || e.Instance != 2 {
					t.Fatalf("%s: marker = %+v, want stream engine, no pid, no command, instance 2", where, e)
				}
			}
		}
		if !found {
			t.Fatalf("%s: no %s marker", where, logEventFileOpened)
		}
	}
	check("live", s.Drain())
	s.SetWatch([]string{"S"}, map[string]uint64{"S": 9})
	check("replay", drainAll(t, s))
}

// TestForStreamSiblingKeepsInstancePrefixAndOwnLineState: a file stream of
// instance k is prefixed like its process output, and the sibling tracks its
// own position in a line -- a process write ending mid-line must not swallow the
// prefix of the file's next line.
func TestForStreamSiblingKeepsInstancePrefixAndOwnLineState(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	p := s.newInstanceProc("S", 2)
	f := p.ForStream("engine")
	if f.gen != p.gen || f.instance != 2 || f.specID != "S" || f.store != s || f.stream != "engine" {
		t.Fatalf("sibling = %+v, want the generation's identity on stream engine", f)
	}
	p.Write([]byte("proc-no-newline"))
	f.Write([]byte("a\nb"))
	if got := s.tailStream("S", "engine", p.gen, 1024); got != "[#2] a\n[#2] b" {
		t.Fatalf("file text = %q", got)
	}
	if got := s.tail("S", p.gen, 1024); got != "[#2] proc-no-newline" {
		t.Fatalf("process text = %q", got)
	}
	nilStore := (&procLog{}).ForStream("engine")
	nilStore.Write([]byte("x"))
	nilStore.Mark(logEventFileOpened)
}

// TestForgetReleasesEveryStream: Retain and leaving the watch set release every
// stream of the spec -- retention bytes back to the total, live queues gone.
func TestForgetReleasesEveryStream(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	p := s.newProc("S")
	s.SetWatch([]string{"S"}, nil)
	s.Drain()
	p.Write([]byte("p"))
	p.ForStream("engine").Write([]byte("e"))
	s.gap(p.ForStream("other"), 5)

	s.SetWatch(nil, nil)
	s.mu.Lock()
	if n := len(s.pending) + len(s.pendingBytes) + len(s.pendingDropped); n != 0 {
		t.Errorf("unwatching left %d live-queue entries behind", n)
	}
	s.mu.Unlock()

	s.Retain(nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used != 0 || len(s.logs) != 0 {
		t.Fatalf("after Retain(nil): used = %d, %d logs, want nothing", s.used, len(s.logs))
	}
}

// TestLogWireReservesCoverTheStream extends TestLogWireReservesAreNeverUnderTheTruth
// to the stream field: an entry naming its stream, and the gap-only entry a
// live drop synthesizes for a stream, must both fit what takeStreamChunk
// reserved.
func TestLogWireReservesCoverTheStream(t *testing.T) {
	stream := strings.Repeat("<", 32) // escape-heavy, at the name length cap
	widest := logRecord{
		gen: 1, pid: 1 << 62, at: mustParseWidestTime(t), text: "out <&>\n\x00\xff",
		exitCode: math.MinInt32, instance: math.MinInt64, stream: stream, dropped: 1 << 61,
	}
	marker := widest
	marker.text, marker.event, marker.dropped = "", logEventFileNotPermitted, 0
	for name, recs := range map[string][]logRecord{
		"output":    {widest},
		"marker":    {marker},
		"drop-only": nil,
		"both":      {widest, marker, widest},
	} {
		t.Run(name, func(t *testing.T) {
			specID := strings.Repeat("s", 128)
			entries, rest, used := takeStreamChunk(specID, stream, recs, 1<<61, maxLogBatchBytes)
			if rest != nil {
				t.Fatalf("unexpected remainder: %+v", rest)
			}
			for _, e := range entries {
				if e.Stream != stream {
					t.Fatalf("entry %+v does not name its stream", e)
				}
			}
			raw, err := json.Marshal(LogBatch{SpecID: specID, Scrollback: true, More: true, Entries: entries})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if len(raw) > used {
				t.Fatalf("the batch marshals to %d bytes but only %d were reserved", len(raw), used)
			}
			for _, r := range recs {
				raw, _ := json.Marshal(entryOf(r))
				if n := len(raw) + 1; n > entryWireMax(r) {
					t.Fatalf("an entry marshals to %d bytes with its comma but entryWireMax reserves %d", n, entryWireMax(r))
				}
			}
		})
	}
}

// TestAGapNamesNoInstanceUntilTextJoinsIt: a gap belongs to no instance
// (LogEntry's doc), so a gap alone names none; the text that follows it in the
// same generation joins it and the entry then names that text's instance.
func TestAGapNamesNoInstanceUntilTextJoinsIt(t *testing.T) {
	s := newTestLogStore(t, minLogBufferBytes, 0)
	f := s.newInstanceProc("S", 2).ForStream("engine")
	s.gap(f, 10)
	s.SetWatch([]string{"S"}, nil)
	alone := drainAll(t, s)
	if len(alone) != 1 || len(alone[0].Entries) != 1 {
		t.Fatalf("replay = %+v, want one gap entry", alone)
	}
	if e := alone[0].Entries[0]; e.DroppedBytes != 10 || e.Instance != 0 || e.Stream != "engine" || e.Text != "" {
		t.Fatalf("gap entry = %+v, want 10 bytes on engine naming no instance", e)
	}

	f.Write([]byte("after\n"))
	s.SetWatch([]string{"S"}, map[string]uint64{"S": 1})
	joined := drainAll(t, s)
	var eng []LogEntry
	for _, b := range joined {
		if b.Scrollback { // the live cut owed to the viewer already watching goes first
			eng = append(eng, b.Entries...)
		}
	}
	if len(eng) != 1 || eng[0].DroppedBytes != 10 || eng[0].Instance != 2 || eng[0].Text != "[#2] after\n" {
		t.Fatalf("replay = %+v, want instance 2's prefixed text carrying the gap", eng)
	}
}
