// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"op-ai-server-agent/internal/gwapi"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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
//
// AND WHAT THIS FILE GAINED SINCE: the RESOLVED LAUNCH COMMAND (command.go).
// Output alone answers "what did it print", not "what did it print THIS
// from" -- and every interesting value in a spec (${PORT}, ${MODEL},
// ${HOST_GPU_IDS}, ${AGENT_ENV:NAME}) is resolved at launch and was, until
// then, dropped immediately after exec.Command consumed it. The masked command
// is a typed FIELD ON THE GENERATION'S OPENING MARKER, so it travels with the
// boundary that already carries the pid and needs no attribution rule of its
// own -- see ResolvedCommand for why that placement, and what it costs when a
// marker is evicted.

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

	// maxLogBatchBytes bounds ONE marshaled LogBatch, and it is the bound that
	// makes this file's output READABLE BY THE GATEWAY at all.
	//
	// The gateway installs gwapi.MaxWSFrameBytes as its inbound SetReadLimit,
	// and coder/websocket answers a frame one byte over it by failing the read
	// and closing 1009 -- taking down the ONE connection telemetry, the reports,
	// the runtime_config push and the certificate doorbell also ride. So the
	// limit that matters is not "how much output did we retain" but "how many
	// bytes does the marshaled frame come to", and those are not the same
	// number: encoding/json escapes a newline to two bytes and each of `<`, `>`,
	// `&`, and every control byte to six, and every entry pays a JSON envelope
	// against a per-record charge of logRecordOverhead that never modeled it.
	//
	// This existed as a defect precisely because the two constants were equal
	// and unrelated: DefaultLogBufferBytes (1 MiB) and the gateway's read limit
	// (1 MiB). trimLocked holds a busy spec at exactly the retention cap, so its
	// scrollback was STRUCTURALLY over the wire cap -- measured at 1,059,609 B
	// for ordinary 86-column loader lines, 1,200,128 B with ANSI colour and
	// 1,873,702 B for a chat template containing `<|im_start|>`. Deriving this
	// from the frame limit instead of from the retention setting is what makes
	// the rule hold when an operator raises RuntimeLogBufferBytes, which
	// config-env.md and the README actively invite them to do.
	//
	// Enforced by CONSTRUCTION, not by inspection: takeChunk sizes every batch
	// against a per-byte UPPER BOUND on the JSON encoding (jsonStringMaxBytes),
	// so there is no "headroom factor" to be wrong about and no input -- escape-
	// heavy, invalid UTF-8, hostile -- that can breach it. Text too long for the
	// remaining room is split on a rune boundary rather than deferred, so a
	// chunk always makes progress.
	maxLogBatchBytes = int(gwapi.MaxWSFrameBytes) - logFrameEnvelopeBytes
	// logFrameEnvelopeBytes reserves the {"type":"runtime_log","data":...}
	// wrapper internal/client puts around a marshaled batch (30 bytes; rounded
	// up), since the gateway's read limit applies to the whole frame.
	logFrameEnvelopeBytes = 64
	// logBatchWireOverhead reserves a LogBatch's own JSON keys and punctuation
	// -- {"spec_id":…,"scrollback":true,"scrollback_more":true,"entries":[…]} --
	// excluding the spec id string, which takeChunk measures.
	logBatchWireOverhead = 96
	// logEntryWireOverhead reserves ONE LogEntry's keys, punctuation and every
	// numeric or timestamp field at its widest (a 20-digit pid, an RFC3339Nano
	// timestamp, a 20-digit dropped_bytes, an 11-digit exit code): 162 bytes
	// exactly, rounded up. The two variable-length fields it does NOT cover --
	// text and the resolved command -- are measured.
	logEntryWireOverhead = 192
	// logCommandWireOverhead reserves a ResolvedCommand's own keys and
	// punctuation, its two bools included; its strings are measured.
	logCommandWireOverhead = 96
)

// Log event kinds. These are the ONLY synthesized entries in the stream, and
// they carry no free text: the portal owns the wording (and its
// translation), so nothing the gateway or the portal renders as a marker can
// ever be forged by a process writing a convincing-looking line to its own
// stderr.
const (
	logEventStarted = "started"
	logEventExited  = "exited"
	// logEventStartFailed opens a generation that never became a process: the
	// exec itself failed (a missing binary, an unusable work_dir), so there is
	// no pid, there will be no output, and there is no exit code to come.
	//
	// It exists because the resolved command hangs on an OPENING marker, and
	// this outcome has to have one: a spec that cannot start prints nothing at
	// all, which makes the command the entire content of the log view and this
	// exactly the case an operator opens it for. Reusing logEventStarted with
	// pid 0 was the alternative and it would have been a lie -- that marker
	// means "output begins here, from this pid".
	logEventStartFailed = "start_failed"
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
	event    string // "" for process output; one of the logEvent* constants
	exitCode int
	// command is set ONLY on an opening marker (logEventStarted /
	// logEventStartFailed): the masked launch command of the generation this
	// marker opens. Attaching it here rather than keeping it per spec is what
	// makes attribution structural -- see ResolvedCommand's doc.
	command *ResolvedCommand
}

// size is the record's weight against a buffer capacity: its text, any command
// it carries, plus a fixed per-record charge (see logRecordOverhead). Charging
// the command is what keeps the per-spec capacity an honest bound now that a
// marker is no longer necessarily tiny.
func (r logRecord) size() int { return len(r.text) + r.command.bytes() + logRecordOverhead }

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
// Command is present only on an opening marker ("started"/"start_failed") and
// carries that generation's resolved, masked launch command (command.go). A
// reader renders it as part of the marker block it belongs to: it describes the
// process whose output follows, and only that one.
type LogEntry struct {
	PID          int              `json:"pid,omitempty"`
	At           time.Time        `json:"at"`
	Text         string           `json:"text,omitempty"`
	DroppedBytes int64            `json:"dropped_bytes,omitempty"`
	Event        string           `json:"event,omitempty"`
	ExitCode     int              `json:"exit_code,omitempty"`
	Command      *ResolvedCommand `json:"command,omitempty"`
}

// LogBatch is the payload of one agent->gateway runtime_log frame: the
// entries accumulated for ONE spec since the previous flush, or one CHUNK of
// that spec's history replay.
//
// Scrollback marks a batch as part of the one-shot history replay a subscribe
// produces, and it is load-bearing rather than cosmetic: it is what tells a
// reader to RESET its view instead of appending, because appending a replay to
// what is already on screen would duplicate the history. An EMPTY scrollback
// batch is itself the answer to a question the operator would otherwise have to
// guess at -- "the retained buffer is empty", which is what an agent restart
// leaves behind -- as opposed to "nothing has arrived yet".
//
// More says "another chunk of THIS replay follows". A retained history no
// longer necessarily fits one frame (see maxLogBatchBytes), so a replay is a
// SEQUENCE of batches, every one of them flagged Scrollback and every one but
// the last also flagged More.
//
// THE TWO FLAGS DO NOT MEAN THE SAME THING ON BOTH HOPS, and the difference is
// deliberate. Here, agent->gateway, Scrollback means "part of a replay" -- it
// is the routing key that lets the gateway hand a replay only to the viewer
// that asked for one and leave a viewer already watching undisturbed. On the
// gateway->portal hop it keeps its original, narrower meaning, "reset now", and
// the gateway sets it on exactly the first chunk each subscriber receives and
// strips More entirely (runtime_logs.go, runtimeLogSub.deliver). A portal
// therefore still sees one reset followed by appends and needs no notion of
// chunking at all.
//
// The resolved launch command travels INSIDE the entries, on each generation's
// opening marker (LogEntry.Command), not as a field here. A batch is a
// time-slice of one spec's output and can span two generations; a command
// belongs to exactly one, so a batch-level field would need a rule to say which
// -- and the marker already answers that question by construction.
type LogBatch struct {
	SpecID     string     `json:"spec_id"`
	Scrollback bool       `json:"scrollback,omitempty"`
	More       bool       `json:"scrollback_more,omitempty"`
	Entries    []LogEntry `json:"entries"`
}

// specLog is one spec's retained history: a byte-bounded record ring that
// spans every generation of that spec's process, opening markers and their
// resolved commands included.
type specLog struct {
	recs      []logRecord
	bytes     int   // sum of every record's size(), i.e. text and commands PLUS overhead
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
// hold a live queue of at most logPendingBytes (64 KiB), plus -- while a
// history replay is in flight for it -- a scrollback snapshot, which is a
// shallow copy that shares its strings with the retention buffer and so adds
// only record headers unless the buffer churns underneath it. A generation's
// opening marker also carries its masked launch command, capped at
// maxResolvedCommandBytes (16 KiB) and charged against the same per-spec
// capacity as the text (logRecord.size), so it widens no bound. With the
// defaults (1 MiB / 16 MiB) and the one or two specs an operator actually
// watches at a time, that is 16 MiB steady-state and under 17 MiB while
// watching.
//
// Every method is safe for concurrent use. Write in particular is called
// from os/exec's two copying goroutines per process, for every process at
// once; the single mutex covers both the retention append and the live
// queue, which is what lets Drain take a history snapshot and cut that queue
// as one indivisible step against a concurrent write -- and therefore what
// keeps the history from being delivered twice or torn.
type LogStore struct {
	mu sync.Mutex

	perSpec  int
	maxSpecs int

	logs map[string]*specLog

	// watching is the CURRENT desired set, replaced wholesale by SetWatch --
	// the gateway always sends the full set, never a delta (see SetWatch).
	watching map[string]bool
	// watchEpoch is the per-spec snapshot epoch last honoured, straight from
	// the gateway's command. A DIFFERENT epoch for a spec already in the set is
	// the gateway saying "a viewer has arrived; take a fresh snapshot" -- see
	// SetWatch for why arrival, not the set changing, is the thing that has to
	// trigger a replay.
	watchEpoch map[string]uint64

	pending        map[string][]logRecord
	pendingBytes   map[string]int
	pendingDropped map[string]int64

	// scrollbackDue marks a spec whose history has been ASKED for but not yet
	// snapshotted; the value says whether the spec was ALREADY being watched
	// when it was asked for, i.e. whether there is an existing viewer whose live
	// stream must not be interrupted. The snapshot itself is taken in Drain
	// rather than in SetWatch, so that it and the cut of the live queue happen in
	// one locked section -- see Drain for why that atomicity is what stops a
	// re-snapshot from either duplicating or losing the output queued around it.
	scrollbackDue map[string]bool
	// scrollback holds what is LEFT of an in-flight history replay, and
	// scrollbackReady records that one is in flight -- separately, because an
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
		watchEpoch:        make(map[string]uint64),
		pending:           make(map[string][]logRecord),
		pendingBytes:      make(map[string]int),
		pendingDropped:    make(map[string]int64),
		scrollbackDue:     make(map[string]bool),
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
//
// cmd is non-nil only for an OPENING marker, and it is the masked launch
// command of the generation that marker opens (see ResolvedCommand). It is
// carried on the record, so it is retained for exactly as long as that record
// is and is delivered wherever that record is -- scrollback and live alike --
// with no separate bookkeeping.
func (s *LogStore) mark(specID string, gen uint64, pid int, event string, exitCode int, cmd *ResolvedCommand) {
	if s == nil || specID == "" {
		return
	}
	rec := logRecord{gen: gen, pid: pid, at: s.now(), event: event, exitCode: exitCode, command: cmd}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(specID, rec)
	s.queueLocked(specID, rec)
}

// logForLocked returns specID's buffer, creating it (and enforcing the total
// ceiling) on first use.
func (s *LogStore) logForLocked(specID string) *specLog {
	l := s.logs[specID]
	if l == nil {
		l = &specLog{}
		s.logs[specID] = l
		s.evictLocked(specID)
	}
	return l
}

// appendLocked adds rec to specID's retained history, coalescing consecutive
// same-generation output and evicting from the front to stay within the
// per-spec capacity.
func (s *LogStore) appendLocked(specID string, rec logRecord) {
	l := s.logForLocked(specID)
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
	s.forgetStreamLocked(specID)
}

// forgetStreamLocked drops everything the STREAMING half holds for specID --
// its live queue, any due or in-flight history replay, and the epoch that
// replay was taken for -- leaving the retention buffer alone. It is what a spec
// leaving the watch set gets: there is no one left to deliver to, and holding
// any of it would be memory kept for nobody.
func (s *LogStore) forgetStreamLocked(specID string) {
	delete(s.watchEpoch, specID)
	delete(s.pending, specID)
	delete(s.pendingBytes, specID)
	delete(s.pendingDropped, specID)
	delete(s.scrollbackDue, specID)
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

// SetWatch replaces the set of specs whose output is streamed upward, and
// records the snapshot EPOCH the gateway attached to each of them. The gateway
// always sends the FULL desired set, never a delta: like the runtime_config
// frame this mirrors, every command is self-contained and idempotent, so
// last-one-wins and a dropped frame costs nothing.
//
// A HISTORY REPLAY IS OWED TO A VIEWER, NOT TO A SET TRANSITION, and getting
// that wrong is what this signature exists to fix. Watching is a SET, so the
// second operator to open a log view on the same spec -- or the same operator
// closing and immediately reopening the dialog, or an SSE connection that
// reconnected under a proxy timeout before the gateway noticed the old one --
// produces a byte-identical set, and a rule keyed to "this id is new" then
// skips the snapshot. The arriving viewer's window stays blank while the agent
// believes it delivered, and the portal, seeing live output with no history in
// front of it, states that the agent no longer retains the command -- when the
// agent retains it and merely did not replay it. Two different facts rendered
// identically, and the one rendered is the wrong one.
//
// So the gateway attaches a per-spec epoch and bumps it whenever a VIEWER
// ARRIVES (and for every watched spec of a server on a fresh agent connection).
// A spec whose epoch differs from the one its last snapshot was taken for is
// re-snapshotted even though it is already watched. Compared with !=, never <:
// a gateway restart resets its counters, and "differs" is the honest predicate
// for "not the snapshot you asked for" in both directions. An absent/zero epoch
// from an older gateway degrades to the previous behaviour exactly.
//
// Re-snapshotting must NOT disturb the viewer already watching, which is why
// the replay is routed rather than broadcast: the gateway hands the resulting
// chunks only to subscribers that have not had a history yet (runtime_logs.go).
// The alternative the reviewers named and rejected -- drop the id from the set
// and re-add it -- races that viewer's live stream and tears their output.
//
// The snapshot itself is taken in Drain, not here; see Drain for why it has to
// happen in the same locked section as the cut of the live queue.
//
// A spec that leaves the set has its queue and any replay discarded
// immediately -- there is no one left to deliver them to.
func (s *LogStore) SetWatch(specIDs []string, epochs map[string]uint64) {
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
			s.forgetStreamLocked(id)
		}
	}
	for id := range next {
		if s.watching[id] && s.watchEpoch[id] == epochs[id] {
			continue
		}
		s.watchEpoch[id] = epochs[id]
		// Supersede any replay still in flight: its snapshot is older than the
		// one this epoch asks for, and half of it followed by half of a fresher
		// one is the torn history the flags exist to prevent.
		delete(s.scrollback, id)
		delete(s.scrollbackDropped, id)
		delete(s.scrollbackReady, id)
		s.scrollbackDue[id] = s.watching[id]
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

// Drain takes what is queued for the watched specs, oldest first, and returns
// it as at most one batch per spec. It is called on a fixed flush cadence by
// the agent's run loop -- the SINGLE goroutine that writes to the gateway
// WebSocket -- so the batching here is also the rate limit. Exactly what it
// bounds, since the previous statement of it was false in two ways:
//
//   - at most ONE batch per spec per interval, replay chunks included;
//   - at most logDrainBudget bytes per interval of LIVE output across all
//     specs, visited round-robin;
//   - plus at most maxLogBatchBytes of HISTORY REPLAY per interval, fleet-wide,
//     in chunks of at most maxLogBatchBytes each. A replay is one-shot and only
//     ever follows a viewer arriving, so it has its own budget rather than
//     competing with live output for one -- but it is no longer unbounded,
//     which is what made a full retention buffer produce a single frame the
//     gateway could not read at all.
//
// THE SNAPSHOT IS TAKEN HERE, and the reason is the interaction between a
// re-snapshot and the live queue. Everything queued for a spec at the instant
// of the snapshot is also IN that snapshot (write appends to both under one
// lock), so a re-snapshot for an arriving viewer leaves the queue holding a
// duplicate of the snapshot's tail -- output the viewer already watching still
// needs delivered, and the arriving one must not see twice. Taking the snapshot
// and cutting the queue in the SAME locked section resolves it exactly: the cut
// goes out as an ordinary live batch (the watching viewer's stream is not
// interrupted), the replay carries the same records as history for the arriving
// one, and the gateway's per-subscriber routing keeps each from seeing the
// other's copy. Nothing can land between the two.
//
// While a replay is in flight for a spec, that spec's live output WAITS behind
// it -- torn history is a worse outcome than a delayed tail, and the wait is
// bounded (the default 1 MiB buffer replays in about two flush intervals) and
// honest (past the per-spec queue cap, bytes are dropped and counted).
//
// Returns nil when there is nothing to send, which on an unwatched fleet is
// always.
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
	replayBudget := maxLogBatchBytes
	var out []LogBatch
	for _, id := range ordered {
		if hadViewer, due := s.scrollbackDue[id]; due {
			if b, ok := s.startReplayLocked(id, hadViewer); ok {
				// The queue held output an EXISTING viewer is still owed. It
				// goes out as a live batch first; the replay begins next
				// interval.
				out = append(out, b)
				continue
			}
		}
		if s.scrollbackReady[id] {
			if replayBudget <= 0 {
				// Another spec's replay has taken this interval's share. The
				// rotation above brings this one round soon, and its snapshot
				// is already taken, so nothing it holds can go stale.
				continue
			}
			b, used := s.replayChunkLocked(id)
			replayBudget -= used
			out = append(out, b)
			continue
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
		entries, rest, _ := takeChunk(id, q, dropped, maxLogBatchBytes)
		budget -= s.pendingBytes[id]
		delete(s.pendingDropped, id)
		if rest == nil {
			delete(s.pending, id)
			delete(s.pendingBytes, id)
		} else {
			// A live queue over the frame cap is unreachable at the sizes
			// queueLocked admits, but it is bounded by CONSTRUCTION rather than
			// by that argument: what did not fit stays queued and goes next
			// interval, in order, with nothing lost.
			kept := 0
			for _, r := range rest {
				kept += r.size()
			}
			budget += kept
			s.pending[id] = rest
			s.pendingBytes[id] = kept
		}
		out = append(out, LogBatch{SpecID: id, Entries: entries})
	}
	return out
}

// startReplayLocked snapshots specID's retained history and cuts its live queue
// in one step -- see Drain for why those two must not be separable.
//
// What becomes of the cut depends on whether anyone was ALREADY watching this
// spec. Everything the queue holds is also in the snapshot just taken (write
// appends to both, under this lock), so:
//
//   - a FIRST subscribe discards it: there is no viewer it belongs to, and the
//     replay carries every one of those bytes as history;
//   - a RE-snapshot, for a second viewer arriving, emits it as an ordinary live
//     batch, returned here so the caller sends it before the replay begins.
//     That is output the viewer already watching is still owed, and it must
//     neither be dropped (a silent gap for them) nor left queued behind the
//     replay (a duplicate of the replay's tail for the viewer arriving).
func (s *LogStore) startReplayLocked(specID string, hadViewer bool) (LogBatch, bool) {
	delete(s.scrollbackDue, specID)
	if l := s.logs[specID]; l != nil {
		// A shallow copy: strings are immutable and trimLocked replaces rather
		// than mutates them, so this shares the text with the retention buffer
		// instead of duplicating a megabyte.
		s.scrollback[specID] = append([]logRecord(nil), l.recs...)
		s.scrollbackDropped[specID] = l.dropped
	}
	s.scrollbackReady[specID] = true

	q := s.pending[specID]
	dropped := s.pendingDropped[specID]
	delete(s.pending, specID)
	delete(s.pendingBytes, specID)
	delete(s.pendingDropped, specID)
	if !hadViewer || (len(q) == 0 && dropped == 0) {
		return LogBatch{}, false
	}
	// Chunked like any other batch; a remainder is dropped rather than requeued
	// because the snapshot just taken contains every one of these records
	// already, so the arriving viewer loses nothing and the watching one sees
	// the same gap it would have seen from a full queue -- counted, not silent.
	entries, rest, _ := takeChunk(specID, q, dropped, maxLogBatchBytes)
	if len(rest) > 0 {
		lost := int64(0)
		for _, r := range rest {
			lost += int64(len(r.text))
		}
		s.pendingDropped[specID] = lost
	}
	return LogBatch{SpecID: specID, Entries: entries}, true
}

// replayChunkLocked emits the next chunk of specID's in-flight history replay,
// flagging More while any of it is left, and reports the wire bytes it used
// against the caller's fleet-wide replay budget. Every chunk carries Scrollback:
// it is the gateway's routing key for "this is history, for whoever asked for
// it".
func (s *LogStore) replayChunkLocked(specID string) (LogBatch, int) {
	entries, rest, used := takeChunk(specID, s.scrollback[specID], s.scrollbackDropped[specID], maxLogBatchBytes)
	// The eviction count belongs to the FIRST chunk only: it means "bytes
	// missing immediately before this entry", and only the head of the history
	// has anything missing in front of it.
	delete(s.scrollbackDropped, specID)
	if rest == nil {
		delete(s.scrollback, specID)
		delete(s.scrollbackReady, specID)
		return LogBatch{SpecID: specID, Scrollback: true, Entries: entries}, used
	}
	s.scrollback[specID] = rest
	return LogBatch{SpecID: specID, Scrollback: true, More: true, Entries: entries}, used
}

// entryOf converts one retained record to its wire entry.
//
// r.command is shared, not copied: it is built once by startProcess and never
// written again, exactly like the immutable strings beside it, so the same
// reasoning that makes the shallow scrollback snapshot safe applies here.
func entryOf(r logRecord) LogEntry {
	return LogEntry{PID: r.pid, At: r.at, Text: r.text, Event: r.event, ExitCode: r.exitCode, Command: r.command}
}

// withDropped attaches dropped (bytes lost immediately BEFORE the first entry)
// to that first entry. A non-zero drop with no entries still produces one: the
// loss is the whole message, and swallowing it would turn a gap back into
// silence.
func withDropped(out []LogEntry, dropped int64) []LogEntry {
	if dropped <= 0 {
		return out
	}
	if len(out) == 0 {
		return []LogEntry{{DroppedBytes: dropped}}
	}
	out[0].DroppedBytes = dropped
	return out
}

// takeChunk converts the longest prefix of recs whose MARSHALED LogBatch is
// guaranteed to fit limit, and returns whatever is left over (nil when nothing
// is). It is the single place the frame guarantee of maxLogBatchBytes is made.
//
// "Guaranteed" is meant literally. Every variable-length field is measured with
// jsonStringMaxBytes, a per-byte UPPER bound on what encoding/json will emit,
// and every fixed field is reserved at its widest -- so the answer is never
// under the truth, for any input, and no escaping factor has to be guessed at.
// The price is over-chunking for text that is heavily multi-byte UTF-8 (an
// extra frame or two), which costs nothing an operator can see.
//
// A record that does not fit the remaining room has its TEXT split on a rune
// boundary rather than being deferred, so a chunk always makes progress and a
// split never manufactures invalid UTF-8 out of a character the process
// actually printed. A marker record cannot be split; it does not need to be,
// because its size is bounded by maxResolvedCommandBytes (16 KiB) plus its
// envelope, two orders below the limit.
func takeChunk(specID string, recs []logRecord, dropped int64, limit int) (entries []LogEntry, rest []logRecord, used int) {
	fixed := logBatchWireOverhead + jsonStringMaxBytes(specID)
	if dropped > 0 {
		// withDropped synthesizes an entry when there is nothing to stamp the
		// loss onto, so the room for one has to be reserved before the loop.
		fixed += logEntryWireOverhead
	}
	budget := limit - fixed
	taken := 0
	for taken < len(recs) {
		cost := entryWireMax(recs[taken])
		if cost > budget {
			break
		}
		budget -= cost
		taken++
	}
	entries = make([]LogEntry, 0, taken+1)
	for _, r := range recs[:taken] {
		entries = append(entries, entryOf(r))
	}
	rest = recs[taken:]
	if len(rest) > 0 {
		switch head, tail, ok := splitRecord(rest[0], budget); {
		case ok:
			entries = append(entries, entryOf(head))
			budget -= entryWireMax(head)
			rest = append([]logRecord{tail}, rest[1:]...)
		case taken == 0:
			// Unsplittable and alone. Unreachable at the sizes this file
			// produces (see the doc above); emitting it is still better than
			// stalling the spec's stream forever on a record that never fits.
			entries = append(entries, entryOf(rest[0]))
			budget -= entryWireMax(rest[0])
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		rest = nil
	}
	// budget started at limit-fixed and lost one entry's cost at a time, so
	// what is left says exactly how much wire this chunk came to.
	return withDropped(entries, dropped), rest, limit - budget
}

// splitRecord cuts as much of r's text as fits budget off the front, returning
// the two halves. Only plain output splits: a marker carries a generation
// boundary (and possibly its command) that has no halves.
//
// Both halves share the retained string rather than copying it -- the record
// they came from is still in the retention buffer holding it alive, so a clone
// here would only make a second copy of prompt-bearing text.
func splitRecord(r logRecord, budget int) (head, tail logRecord, ok bool) {
	if r.event != "" || r.command != nil || r.text == "" {
		return logRecord{}, logRecord{}, false
	}
	room := budget - logEntryWireOverhead - 2 // the two quotes
	n, cost := 0, 0
	for n < len(r.text) {
		c := jsonByteMaxBytes(r.text[n])
		if cost+c > room {
			break
		}
		cost += c
		n++
	}
	for n > 0 && n < len(r.text) && !utf8.RuneStart(r.text[n]) {
		n--
	}
	if n == 0 {
		return logRecord{}, logRecord{}, false
	}
	head, tail = r, r
	head.text, tail.text = r.text[:n], r.text[n:]
	return head, tail, true
}

// entryWireMax is an upper bound on the bytes one LogEntry costs inside a
// marshaled LogBatch, its trailing comma included.
func entryWireMax(r logRecord) int {
	n := logEntryWireOverhead
	if r.text != "" {
		n += jsonStringMaxBytes(r.text)
	}
	if r.event != "" {
		n += jsonStringMaxBytes(r.event)
	}
	return n + commandWireMax(r.command)
}

// commandWireMax is an upper bound on the bytes a ResolvedCommand costs as an
// entry's "command" object.
func commandWireMax(c *ResolvedCommand) int {
	if c == nil {
		return 0
	}
	n := logCommandWireOverhead + jsonStringMaxBytes(c.Binary) + jsonStringMaxBytes(c.WorkDir)
	for _, a := range c.Args {
		n += jsonStringMaxBytes(a) + 1 // + the separating comma
	}
	for _, e := range c.Env {
		n += jsonStringMaxBytes(e) + 1
	}
	return n
}

// jsonStringMaxBytes is an upper bound on what encoding/json emits for s as a
// JSON string, the two quotes included. Never under the truth, and exact for
// plain ASCII output, which is what a model server almost always prints.
func jsonStringMaxBytes(s string) int {
	n := 2
	for i := 0; i < len(s); i++ {
		n += jsonByteMaxBytes(s[i])
	}
	return n
}

// jsonByteMaxBytes is the per-byte half of jsonStringMaxBytes.
//
// The classes, and why each is what it is: \n \r \t and the two mandatory
// escapes cost two bytes; every other control byte, and each of < > & (which
// encoding/json escapes by default so a JSON document is safe to embed in
// HTML), costs six as \u00XX. A byte at or above 0x80 is charged six because
// this function deliberately does not decode UTF-8: valid multi-byte sequences
// pass through at one byte each and U+2028/U+2029 become six for three, but an
// INVALID byte becomes � -- six bytes for one -- and that is the case the
// bound has to cover.
func jsonByteMaxBytes(c byte) int {
	switch {
	case c == '\n', c == '\r', c == '\t', c == '"', c == '\\':
		return 2
	case c < 0x20, c == '<', c == '>', c == '&', c >= 0x80:
		return 6
	default:
		return 1
	}
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

// Started records the generation boundary that opens this process's output,
// together with the resolved command it was launched with (already masked --
// see command.go). The two are recorded in one call because they are one fact:
// "this pid, running this command, from here on".
func (p *procLog) Started(pid int, cmd ResolvedCommand) {
	p.pid.Store(int64(pid))
	if p.store != nil {
		p.store.mark(p.specID, p.gen, pid, logEventStarted, 0, &cmd)
	}
}

// StartFailed records the opening marker of a generation that never became a
// process: the exec itself failed, so there is no pid and there will be no
// output -- the command it carries is the whole content of the log view for a
// spec that cannot start, which is the case an operator opens it for most
// often. Deliberately its own event kind rather than a pid-0 "started", which
// would claim output begins here; see logEventStartFailed.
func (p *procLog) StartFailed(cmd ResolvedCommand) {
	if p.store != nil {
		p.store.mark(p.specID, p.gen, 0, logEventStartFailed, 0, &cmd)
	}
}

// Exited records the generation boundary that closes it. Together with
// Started these are what makes a crash loop READABLE: the appended history of
// several attempts, with the exit code of each one in its right place.
func (p *procLog) Exited(exitCode int) {
	if p.store != nil {
		p.store.mark(p.specID, p.gen, int(p.pid.Load()), logEventExited, exitCode, nil)
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
