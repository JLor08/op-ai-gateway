// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file owns everything the agent remembers, and everything it streams,
// about what a managed model process printed.
//
// THE SAFETY RULE, WHICH NOTHING HERE MAY WEAKEN. A model server writes
// whatever it likes to its own stdout/stderr, and for an inference server
// that routinely includes fragments of prompt text. Every byte that reaches
// this file is therefore treated as potentially prompt-bearing: it is kept
// in memory, bounded, and NEVER written to disk, never sent to the gateway's
// database, and never put into a log line on either side. The gateway relays
// it to a watching operator in memory and forgets it (see
// gateway/backend/internal/gateway/runtime_logs.go, whose registry doc
// carries the same rule for the other half of the path). A caller must not
// persist anything this file returns anywhere durable.
//
// WHAT CHANGED IN T3 (log streaming), and why the shape is what it is:
//
//   - Retention moved from the PROCESS to the SPEC. The buffer used to hang
//     off runningProc, so a crash destroyed the output at exactly the moment
//     it became valuable -- all that survived was the ~2 KiB snapshotted into
//     LastError.StderrTail. An operator staring at a `crashed` row needs the
//     output that led to the crash, so the buffer must outlive the process
//     that filled it.
//   - Generations APPEND, they do not replace. For a crash loop -- the case
//     an operator is most often looking at -- the pattern ACROSS attempts is
//     the diagnosis ("it dies at the same layer every time" vs. "the third
//     one got further"), and replacing on restart throws exactly that
//     comparison away. The boundary stays visible because it is recorded as
//     a typed marker (logEventStarted/logEventExited with the pid and exit
//     code), never as synthesized text that would be indistinguishable from
//     something the process itself printed.
//   - Capacity is an OPERATOR setting, not a constant. Memory on an AI server
//     is the operator's tradeoff (config.Config.RuntimeLogBufferBytes /
//     RuntimeLogBufferTotalBytes -- local file/env only, never gateway-
//     supplied, exactly like runtime_allowed_binaries).
//   - Streaming is ON DEMAND. Collection above is unconditional; the live
//     fan-out below does nothing at all until the gateway names a spec in
//     SetWatch, so an unwatched fleet produces no log traffic.

const (
	// DefaultLogBufferBytes is the per-spec retention default: 1 MiB.
	//
	// Sized against what actually has to fit. The predecessor was 64 KiB,
	// chosen for "the one CUDA error: out of memory line and its
	// neighbours", which is roughly 500-800 lines -- often less than ONE
	// vLLM or llama.cpp startup, so the interesting part (the config dump,
	// the weight-loading progress, the layer that failed) had already
	// scrolled out by the time the failure line appeared. A model that
	// fails during a long load is precisely the case this feature exists
	// for, so the buffer has to hold a whole startup plus its context, and
	// -- because generations append -- several attempts of a crash loop.
	// 1 MiB does that with room to spare, and is a rounding error on a host
	// whose reason for existing is holding tens of gigabytes of weights.
	DefaultLogBufferBytes = 1 << 20
	// DefaultLogBufferTotalBytes is the default ceiling across ALL specs:
	// 16 MiB, i.e. the per-spec default times sixteen retained specs. The
	// per-spec number alone is not a bound -- a server with twenty specs
	// would be twenty times it -- and this is a host where memory is the
	// point, so the total is what an operator actually needs to be able to
	// reason about. See LogStore's doc for the exact worst case.
	DefaultLogBufferTotalBytes = 16 << 20
	// minLogBufferBytes floors an operator's per-spec setting. Below this a
	// buffer cannot hold even one 32 KiB pipe read plus context, which makes
	// the feature actively misleading rather than merely small.
	minLogBufferBytes = 64 << 10

	// logCoalesceTarget is how large a record may grow by absorbing the
	// writes that follow it. Without coalescing a line-buffered child turns
	// one megabyte of retention into ~1M records of a few bytes each, and
	// the per-record bookkeeping -- not the text -- becomes the memory
	// number. With it, record count is bounded by capacity/logCoalesceTarget
	// plus one marker per generation.
	logCoalesceTarget = 8 << 10
	// logRecordOverhead is the per-record weight charged against a buffer's
	// capacity, so a burst of zero-length marker records (a fast crash loop)
	// is bounded by the SAME setting the text is, rather than growing the
	// record slice without limit. Deliberately generous versus the real
	// struct size: it stands in for the slice slot, the string header, and
	// the allocator's rounding.
	logRecordOverhead = 64

	// logPendingBytes bounds ONE watched spec's live queue -- what has
	// accumulated since the last Drain (250 ms). Deliberately independent of
	// the retention capacity: this is a rate bound, not a history bound, and
	// 64 KiB per flush window (256 KiB/s for one spec) is already far more
	// than any real model server sustains. Past it, bytes are dropped and
	// COUNTED, and the count travels in the very next entry -- see Drain.
	logPendingBytes = 64 << 10
	// logDrainBudget bounds one Drain across ALL watched specs, so a chatty
	// fleet cannot monopolize the single agent->gateway WebSocket writer that
	// telemetry also uses. At the agent's 250 ms flush cadence this is
	// ~512 KiB/s of log traffic, fleet-wide, and only while someone is
	// actually watching. Specs are visited round-robin so the budget cannot
	// starve whichever spec happens to sort last.
	//
	// It is deliberately LARGER than logPendingBytes, and that relationship is
	// load-bearing rather than a taste: Drain defers a spec whose whole queue
	// does not fit rather than tearing its batch, so a per-spec queue that
	// could exceed the whole budget would be a queue that never drains at all
	// -- output accumulating, overflowing, and reporting a growing gap while
	// the operator's window stays empty.
	logDrainBudget = 128 << 10
)

// Log event kinds. These are the ONLY synthesized entries in the stream, and
// they carry no free text: the portal owns the wording (and its
// translation), so nothing the gateway or the portal renders as a marker can
// ever be forged by a process writing a convincing-looking line to its own
// stderr.
const (
	logEventStarted = "started"
	logEventExited  = "exited"
)

// logRecord is one retained unit of a spec's output history. Text-carrying
// records and lifecycle markers share the type so that the boundary between
// two generations keeps its exact position in the history, including after
// the oldest bytes have been evicted.
type logRecord struct {
	gen      uint64 // generation (one running process) that produced this
	pid      int
	at       time.Time
	text     string
	event    string // "" for process output; logEventStarted/logEventExited
	exitCode int
}

// size is the record's weight against a buffer capacity: its text plus a
// fixed per-record charge (see logRecordOverhead).
func (r logRecord) size() int { return len(r.text) + logRecordOverhead }

// LogEntry is the wire shape of one unit of managed-process output, carried
// inside a runtime_log frame's LogBatch.
//
// DroppedBytes is the overflow marker, and it means exactly one thing
// everywhere it appears: "N bytes the process printed are missing
// immediately before this entry's text". It is produced by all three places
// output can be lost -- eviction from the retention buffer before a
// scrollback snapshot, the per-spec live queue overflowing between two
// flushes, and the gateway's per-subscriber queue overflowing -- and the
// three are deliberately indistinguishable to the reader, because the reader
// only needs the one fact. A gap that renders as silence would be a lie
// about what the process printed, and silence is exactly what an operator is
// trying to interpret.
type LogEntry struct {
	PID          int       `json:"pid,omitempty"`
	At           time.Time `json:"at"`
	Text         string    `json:"text,omitempty"`
	DroppedBytes int64     `json:"dropped_bytes,omitempty"`
	Event        string    `json:"event,omitempty"`
	ExitCode     int       `json:"exit_code,omitempty"`
}

// LogBatch is the payload of one agent->gateway runtime_log frame: the
// entries accumulated for ONE spec since the previous flush.
//
// Scrollback marks the one-shot history replay a subscribe produces, and it
// is load-bearing rather than cosmetic. It tells the portal to RESET its view
// (an agent reconnect delivers a fresh scrollback, and appending it to what
// is already on screen would duplicate the history), and an EMPTY scrollback
// batch is itself the answer to a question the operator would otherwise have
// to guess at: "the retained buffer is empty" -- which is what an agent
// restart leaves behind -- as opposed to "nothing has arrived yet".
type LogBatch struct {
	SpecID     string     `json:"spec_id"`
	Scrollback bool       `json:"scrollback,omitempty"`
	Entries    []LogEntry `json:"entries"`
}

// specLog is one spec's retained history: a byte-bounded record ring that
// spans every generation of that spec's process.
type specLog struct {
	recs      []logRecord
	bytes     int   // sum of every record's size(), i.e. text PLUS overhead
	dropped   int64 // text bytes evicted from the front since this log began
	lastWrite time.Time
}

// LogStore is the agent's per-spec managed-process output: unconditional,
// in-memory, never-persisted retention, plus the on-demand live fan-out the
// gateway drives with SetWatch.
//
// COLLECTION IS UNCONDITIONAL, STREAMING IS NOT. Every byte a managed
// process writes lands in the retention buffer whether or not anyone is
// watching -- that is the whole point: the operator arrives AFTER the
// incident. The live queue underneath is the opposite: it stays empty, and
// Drain returns nothing, until the gateway names a spec in SetWatch, so a
// fleet nobody is looking at generates no log traffic at all.
//
// THE MEMORY BOUND, exactly. Retention is capped at totalBytes: at most
// totalBytes/perSpec spec buffers are kept (the least-recently-written
// unwatched one is evicted to make room), and each is capped at perSpec
// bytes INCLUDING per-record overhead. On top of that, each WATCHED spec can
// hold a live queue of at most logPendingBytes (256 KiB), plus -- for one
// flush interval after a subscribe -- a scrollback snapshot, which is a
// shallow copy that shares its strings with the retention buffer and so adds
// only record headers unless the buffer churns underneath it. With the
// defaults (1 MiB / 16 MiB) and the one or two specs an operator actually
// watches at a time, that is 16 MiB steady-state and under 17 MiB while
// watching.
//
// Every method is safe for concurrent use. Write in particular is called
// from os/exec's two copying goroutines per process, for every process at
// once; the single mutex covers both the retention append and the live
// queue, which is what makes SetWatch's snapshot atomic against a concurrent
// write and therefore what keeps the history from being delivered twice.
type LogStore struct {
	mu sync.Mutex

	perSpec  int
	maxSpecs int

	logs map[string]*specLog

	// watching is the CURRENT desired set, replaced wholesale by SetWatch --
	// the gateway always sends the full set, never a delta (see SetWatch).
	watching map[string]bool

	pending        map[string][]logRecord
	pendingBytes   map[string]int
	pendingDropped map[string]int64

	// scrollback holds the one-shot history snapshot SetWatch queues, and
	// scrollbackReady records that one is due -- separately, because an
	// EMPTY history is a meaningful answer that still has to be delivered.
	scrollback        map[string][]logRecord
	scrollbackDropped map[string]int64
	scrollbackReady   map[string]bool

	nextGen uint64
	// rr rotates Drain's starting point so the per-drain byte budget cannot
	// systematically starve whichever spec sorts last.
	rr int

	now func() time.Time
}

// NewLogStore builds a store with an operator-chosen per-spec capacity and
// total ceiling. Both are clamped into a sane range rather than rejected: a
// misconfigured buffer size must never stop an agent from supervising model
// processes, which is its actual job.
func NewLogStore(perSpecBytes, totalBytes int) *LogStore {
	if perSpecBytes <= 0 {
		perSpecBytes = DefaultLogBufferBytes
	}
	if perSpecBytes < minLogBufferBytes {
		perSpecBytes = minLogBufferBytes
	}
	if totalBytes <= 0 {
		totalBytes = DefaultLogBufferTotalBytes
	}
	if totalBytes < perSpecBytes {
		totalBytes = perSpecBytes
	}
	maxSpecs := totalBytes / perSpecBytes
	if maxSpecs < 1 {
		maxSpecs = 1
	}
	return &LogStore{
		perSpec:           perSpecBytes,
		maxSpecs:          maxSpecs,
		logs:              make(map[string]*specLog),
		watching:          make(map[string]bool),
		pending:           make(map[string][]logRecord),
		pendingBytes:      make(map[string]int),
		pendingDropped:    make(map[string]int64),
		scrollback:        make(map[string][]logRecord),
		scrollbackDropped: make(map[string]int64),
		scrollbackReady:   make(map[string]bool),
		now:               time.Now,
	}
}

// Capacity reports the resolved per-spec and total byte ceilings (after
// clamping), for the one startup log line that tells an operator what their
// setting actually became.
func (s *LogStore) Capacity() (perSpec, total int) {
	if s == nil {
		return 0, 0
	}
	return s.perSpec, s.perSpec * s.maxSpecs
}

// newProc opens a fresh generation for specID and returns the io.Writer that
// startProcess assigns to BOTH exec.Cmd.Stdout and .Stderr. The requirement
// says stdout and stderr both, and they deliberately share one writer so the
// interleaving an operator reads is the interleaving the process produced.
func (s *LogStore) newProc(specID string) *procLog {
	if s == nil {
		return &procLog{}
	}
	s.mu.Lock()
	s.nextGen++
	gen := s.nextGen
	s.mu.Unlock()
	return &procLog{store: s, specID: specID, gen: gen}
}

// write records p as this generation's output and, when the spec is watched,
// queues it for the next Drain. Never blocks on a reader and never fails:
// there is no failure mode for a bounded in-memory append, and a log write
// must never be able to stall the process it is reading from.
func (s *LogStore) write(specID string, gen uint64, pid int, p []byte) {
	if s == nil || specID == "" || len(p) == 0 {
		return
	}
	rec := logRecord{gen: gen, pid: pid, at: s.now(), text: string(p)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(specID, rec)
	s.queueLocked(specID, rec)
}

// mark records a lifecycle marker (a generation boundary) in the same
// history and live queue the output flows through, so its position relative
// to the surrounding output is exact.
func (s *LogStore) mark(specID string, gen uint64, pid int, event string, exitCode int) {
	if s == nil || specID == "" {
		return
	}
	rec := logRecord{gen: gen, pid: pid, at: s.now(), event: event, exitCode: exitCode}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(specID, rec)
	s.queueLocked(specID, rec)
}

// appendLocked adds rec to specID's retained history, coalescing consecutive
// same-generation output and evicting from the front to stay within the
// per-spec capacity.
func (s *LogStore) appendLocked(specID string, rec logRecord) {
	l := s.logs[specID]
	if l == nil {
		l = &specLog{}
		s.logs[specID] = l
		s.evictLocked(specID)
	}
	l.lastWrite = rec.at
	if rec.event == "" && len(l.recs) > 0 {
		last := &l.recs[len(l.recs)-1]
		if last.event == "" && last.gen == rec.gen && len(last.text) < logCoalesceTarget {
			last.text += rec.text
			l.bytes += len(rec.text)
			s.trimLocked(l)
			return
		}
	}
	l.recs = append(l.recs, rec)
	l.bytes += rec.size()
	s.trimLocked(l)
}

// trimLocked drops the oldest bytes until l is back within the per-spec
// capacity, counting exactly how much text it discarded so a later
// scrollback can report it instead of showing a silent gap.
func (s *LogStore) trimLocked(l *specLog) {
	for l.bytes > s.perSpec && len(l.recs) > 0 {
		excess := l.bytes - s.perSpec
		head := &l.recs[0]
		if len(head.text) > excess {
			// strings.Clone, not a bare reslice: a Go string slice keeps the
			// WHOLE backing array alive, which would leave the discarded
			// prefix -- potentially prompt-bearing -- reachable for as long
			// as the remainder lives. Same reasoning the predecessor of this
			// file recorded for its own reallocate-on-overflow.
			head.text = strings.Clone(head.text[excess:])
			l.bytes -= excess
			l.dropped += int64(excess)
			return
		}
		l.dropped += int64(len(head.text))
		l.bytes -= head.size()
		l.recs[0] = logRecord{} // release the string before dropping the slot
		l.recs = l.recs[1:]
	}
}

// evictLocked enforces the TOTAL ceiling by dropping whole spec buffers,
// least-recently-written first and preferring an unwatched one, until the
// store holds at most maxSpecs of them. keep is the spec that just arrived
// and is never the victim.
func (s *LogStore) evictLocked(keep string) {
	for len(s.logs) > s.maxSpecs {
		victim := ""
		var victimAt time.Time
		victimWatched := true
		for id, l := range s.logs {
			if id == keep {
				continue
			}
			watched := s.watching[id]
			// An unwatched buffer always outranks a watched one as a victim;
			// among equals, the least recently written goes first.
			better := victim == "" ||
				(victimWatched && !watched) ||
				(victimWatched == watched && l.lastWrite.Before(victimAt))
			if better {
				victim, victimAt, victimWatched = id, l.lastWrite, watched
			}
		}
		if victim == "" {
			return
		}
		s.forgetLocked(victim)
	}
}

// forgetLocked removes every trace of specID from the store.
func (s *LogStore) forgetLocked(specID string) {
	delete(s.logs, specID)
	delete(s.pending, specID)
	delete(s.pendingBytes, specID)
	delete(s.pendingDropped, specID)
	delete(s.scrollback, specID)
	delete(s.scrollbackDropped, specID)
	delete(s.scrollbackReady, specID)
}

// queueLocked adds rec to specID's live queue, but only while the spec is
// watched -- this is the "no traffic on an unwatched fleet" half of the
// design. Past logPendingBytes the text is dropped and its length counted;
// a MARKER is never dropped, because losing "the process exited, code 1" is
// losing the very fact the operator is reading the stream to find.
func (s *LogStore) queueLocked(specID string, rec logRecord) {
	if !s.watching[specID] {
		return
	}
	if rec.event == "" && s.pendingBytes[specID]+rec.size() > logPendingBytes {
		s.pendingDropped[specID] += int64(len(rec.text))
		return
	}
	q := s.pending[specID]
	if rec.event == "" && len(q) > 0 {
		last := &q[len(q)-1]
		if last.event == "" && last.gen == rec.gen && len(last.text) < logCoalesceTarget {
			last.text += rec.text
			s.pendingBytes[specID] += len(rec.text)
			return
		}
	}
	s.pending[specID] = append(q, rec)
	s.pendingBytes[specID] += rec.size()
}

// SetWatch replaces the set of specs whose output is streamed upward. The
// gateway always sends the FULL desired set, never a delta: like the
// runtime_config frame this mirrors, every command is self-contained and
// idempotent, so last-one-wins and a dropped frame costs nothing.
//
// A spec that joins the set gets its retained history snapshotted RIGHT HERE,
// under the same lock write uses. That is what makes "scrollback, then live"
// exact: no write can land between the snapshot and the first queued entry,
// so nothing is delivered twice and nothing falls between them.
//
// A spec that leaves the set has its queue and any undelivered scrollback
// discarded immediately -- there is no one left to deliver them to, and
// holding them would be memory kept for nobody.
func (s *LogStore) SetWatch(specIDs []string) {
	if s == nil {
		return
	}
	next := make(map[string]bool, len(specIDs))
	for _, id := range specIDs {
		if id != "" {
			next[id] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.watching {
		if !next[id] {
			delete(s.pending, id)
			delete(s.pendingBytes, id)
			delete(s.pendingDropped, id)
			delete(s.scrollback, id)
			delete(s.scrollbackDropped, id)
			delete(s.scrollbackReady, id)
		}
	}
	for id := range next {
		if s.watching[id] {
			continue
		}
		if l := s.logs[id]; l != nil {
			// A shallow copy: strings are immutable and trimLocked replaces
			// rather than mutates them, so this shares the text with the
			// retention buffer instead of duplicating a megabyte.
			s.scrollback[id] = append([]logRecord(nil), l.recs...)
			s.scrollbackDropped[id] = l.dropped
		}
		s.scrollbackReady[id] = true
	}
	s.watching = next
}

// Watching reports the current watch set, sorted -- diagnostics and tests
// only; nothing in the streaming path reads it.
func (s *LogStore) Watching() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.watching))
	for id := range s.watching {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Retain drops the retained history of every spec not in specIDs, called
// from the manager's own config reconciliation: a spec removed from the
// desired configuration has no row left in the portal to open a log view on,
// so keeping its output would be memory held for something that no longer
// exists.
func (s *LogStore) Retain(specIDs []string) {
	if s == nil {
		return
	}
	live := make(map[string]bool, len(specIDs))
	for _, id := range specIDs {
		live[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.logs {
		if !live[id] {
			s.forgetLocked(id)
		}
	}
}

// Drain takes everything queued for the watched specs, oldest first, and
// returns it as one batch per spec. It is called on a fixed flush cadence by
// the agent's run loop -- the SINGLE goroutine that writes to the gateway
// WebSocket -- so the batching here is also the rate limit: at most one frame
// per spec per interval, and at most logDrainBudget bytes per interval across
// all of them, visited round-robin.
//
// A scrollback batch is delivered whole and OUTSIDE the byte budget: it is a
// one-shot snapshot that is already bounded by the per-spec capacity, and
// deferring half of it would produce exactly the torn history the Scrollback
// flag exists to prevent. Returns nil when there is nothing to send, which
// on an unwatched fleet is always.
func (s *LogStore) Drain() []LogBatch {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.watching))
	for id := range s.watching {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	if s.rr >= len(ids) {
		s.rr = 0
	}
	ordered := append(append([]string(nil), ids[s.rr:]...), ids[:s.rr]...)
	s.rr++

	budget := logDrainBudget
	var out []LogBatch
	for _, id := range ordered {
		if s.scrollbackReady[id] {
			recs := s.scrollback[id]
			dropped := s.scrollbackDropped[id]
			delete(s.scrollback, id)
			delete(s.scrollbackDropped, id)
			delete(s.scrollbackReady, id)
			out = append(out, LogBatch{SpecID: id, Scrollback: true, Entries: entriesFrom(recs, dropped)})
		}
		q := s.pending[id]
		dropped := s.pendingDropped[id]
		if len(q) == 0 && dropped == 0 {
			continue
		}
		if s.pendingBytes[id] > budget && budget < logDrainBudget {
			// Leave it queued for the next drain rather than tearing the
			// batch: the rotation above guarantees this spec goes first soon,
			// and the per-spec queue cap keeps the wait bounded (past it,
			// bytes are dropped and counted, never silently lost).
			//
			// The `budget < logDrainBudget` half is what makes that promise
			// unconditional: it means "something has already been taken this
			// round". The FIRST spec of a round is served whatever its size,
			// so a queue that somehow exceeded the full budget (marker records
			// are admitted past the per-spec cap, by design, so that a
			// process-exit boundary can never be the thing that gets dropped)
			// is drained rather than stuck forever.
			continue
		}
		budget -= s.pendingBytes[id]
		delete(s.pending, id)
		delete(s.pendingBytes, id)
		delete(s.pendingDropped, id)
		out = append(out, LogBatch{SpecID: id, Entries: entriesFrom(q, dropped)})
	}
	return out
}

// entriesFrom converts retained records to wire entries, attaching dropped
// (bytes lost immediately BEFORE the first of them) to that first entry. A
// non-zero drop with no records still produces one entry: the loss is the
// whole message, and swallowing it would turn a gap back into silence.
func entriesFrom(recs []logRecord, dropped int64) []LogEntry {
	out := make([]LogEntry, 0, len(recs)+1)
	for _, r := range recs {
		out = append(out, LogEntry{PID: r.pid, At: r.at, Text: r.text, Event: r.event, ExitCode: r.exitCode})
	}
	if dropped > 0 {
		if len(out) == 0 {
			return []LogEntry{{DroppedBytes: dropped}}
		}
		out[0].DroppedBytes = dropped
	}
	return out
}

// tail returns the last n bytes of text written by generation gen, ignoring
// markers and every other generation -- the exact semantics
// LastError.StderrTail had when the buffer still belonged to one process, now
// that the buffer spans all of them.
func (s *LogStore) tail(specID string, gen uint64, n int) string {
	if s == nil || n <= 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.logs[specID]
	if l == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range l.recs {
		if r.gen == gen && r.event == "" {
			b.WriteString(r.text)
		}
	}
	out := b.String()
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// procLog is ONE generation's view of its spec's log: the io.Writer handed to
// exec.Cmd for both stdout and stderr, plus the two lifecycle markers the
// manager records around the process's life.
//
// Write is called concurrently from os/exec's two copying goroutines (one per
// stream), which is why every mutation goes through LogStore's mutex. The pid
// is an atomic because it is stored by the owner goroutine immediately after
// cmd.Start while those two goroutines may already be copying: output written
// in that sliver carries pid 0, and the started marker that follows carries
// the real one.
type procLog struct {
	store  *LogStore
	specID string
	gen    uint64
	pid    atomic.Int64
}

// Write implements io.Writer. It never returns an error: there is no failure
// mode for a bounded in-memory append, and reporting one would make os/exec
// stop copying the stream -- silencing the process the operator is reading.
func (p *procLog) Write(b []byte) (int, error) {
	if p.store != nil {
		p.store.write(p.specID, p.gen, int(p.pid.Load()), b)
	}
	return len(b), nil
}

// Started records the generation boundary that opens this process's output.
func (p *procLog) Started(pid int) {
	p.pid.Store(int64(pid))
	if p.store != nil {
		p.store.mark(p.specID, p.gen, pid, logEventStarted, 0)
	}
}

// Exited records the generation boundary that closes it. Together with
// Started these are what makes a crash loop READABLE: the appended history of
// several attempts, with the exit code of each one in its right place.
func (p *procLog) Exited(exitCode int) {
	if p.store != nil {
		p.store.mark(p.specID, p.gen, int(p.pid.Load()), logEventExited, exitCode)
	}
}

// Tail returns the last n bytes THIS generation wrote (see LogStore.tail) --
// the bounded snapshot that becomes LastError.StderrTail. Its output must
// not be persisted anywhere durable; the gateway keeps it in volatile memory
// only, exactly like the rest of this path.
func (p *procLog) Tail(n int) string {
	if p.store == nil {
		return ""
	}
	return p.store.tail(p.specID, p.gen, n)
}
