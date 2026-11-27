// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"op-ai-server-agent/internal/gwapi"
	"sort"
	"strconv"
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
// ${HOST_GPU_IDS}, ${CONFIG_DIR}, ${AGENT_ENV:NAME}) is resolved at launch and
// was, until then, dropped immediately after exec.Command consumed it. The
// masked command is a typed FIELD ON THE GENERATION'S OPENING MARKER, so it
// travels with the boundary that already carries the pid and needs no
// attribution rule of its own -- see ResolvedCommand for why that placement,
// and what it costs when a marker is evicted.
//
// AND STREAMS (#430). A spec may declare log files its server writes, and the
// agent tails them into the same store. Each spec therefore holds one RING per
// STREAM: stream "" is the process output this file always held, and every
// declared file is a stream named after it. A ring is trimmed against its OWN
// cap, so a file that floods can never push the process output -- or another
// file -- out of the history, and each ring counts its own gap. Everything
// downstream follows the same split: records, entries and the live queue carry
// the stream, a live queue exists per (spec, stream) unit, and the replay walks
// every stream of the spec in one chunk sequence. What does NOT split is the
// total ceiling: it is one byte bound over every ring of every spec (see
// LogStore's doc).

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

	// logPendingBytes bounds ONE watched (spec, stream) unit's live queue --
	// what has accumulated on one stream of one spec since the last Drain
	// (250 ms). Deliberately independent of the retention capacity: this is a
	// rate bound, not a history bound, and 64 KiB per flush window (256 KiB/s
	// for one stream) is already far more than any real model server
	// sustains, and exactly a log-file tailer's read-rate cap. Past it, bytes
	// are dropped and COUNTED, and the count travels in the very next entry --
	// see Drain.
	logPendingBytes = 64 << 10
	// logDrainBudget bounds one Drain across ALL watched specs, so a chatty
	// fleet cannot monopolize the single agent->gateway WebSocket writer that
	// telemetry also uses. At the agent's 250 ms flush cadence this is
	// ~512 KiB/s of log traffic, fleet-wide, and only while someone is
	// actually watching. (spec, stream) units are visited process units first,
	// then file units, each group round-robin among itself, so files never
	// defer a process stream and the budget cannot starve whichever unit of a
	// group happens to sort last (see Drain).
	//
	// It is deliberately LARGER than logPendingBytes, and that relationship is
	// load-bearing rather than a taste: Drain defers a unit whose whole queue
	// does not fit rather than tearing its batch, so a per-unit queue that
	// could exceed the whole budget would be a queue that never drains at all
	// -- output accumulating, overflowing, and reporting a growing gap while
	// the operator's window stays empty. Because the queue is per UNIT, a
	// spec with log files does not change the relation: each of its streams
	// is a queue of its own, each one fitting a fresh budget whole.
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
	// (1 MiB). trimRing holds a busy ring at exactly the retention cap, so its
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
	// timestamp, a 20-digit dropped_bytes, an 11-digit exit code, a 20-digit
	// instance): 194 bytes exactly, rounded up. The two variable-length fields
	// it does NOT cover -- text and the resolved command -- are measured.
	logEntryWireOverhead = 224
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

	// File-stream events (#430): the closed set a log-file tailer records on
	// its own stream. Like the generation markers they carry no text -- the
	// portal owns the wording -- and they are valid only on an entry that names
	// a stream. The first five are markers inside a file's history, the last
	// four are statuses (why the tab has nothing, or nothing more, to show).
	logEventFileOpened       = "file_opened"
	logEventFileRotated      = "file_rotated"
	logEventFileTruncated    = "file_truncated"
	logEventFileLineCut      = "file_line_cut"
	logEventFileStale        = "file_stale"
	logEventFileWaiting      = "file_waiting"
	logEventFileNotPermitted = "file_not_permitted"
	logEventFileBinary       = "file_binary"
	logEventFileMissing      = "file_missing"
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
	// instance is the index of the spec instance whose generation produced
	// this record (#340): 0 for the spec's own process. It is the same for
	// every record of one generation, which is what lets same-generation
	// coalescing ignore it.
	instance int
	// command is set ONLY on an opening marker (logEventStarted /
	// logEventStartFailed): the masked launch command of the generation this
	// marker opens. Attaching it here rather than keeping it per spec is what
	// makes attribution structural -- see ResolvedCommand's doc.
	command *ResolvedCommand
	// stream names the ring this record belongs to: "" for process output, a
	// declared log file's name otherwise. Every record of one ring carries the
	// ring's own name, so an entry built from it can always say where it came
	// from.
	stream string
	// dropped is a gap: the number of bytes of this stream that are missing
	// IMMEDIATELY BEFORE this record's text. It is set by LogStore.gap at the
	// position the bytes were skipped (the record then has no text until the
	// next write of its generation coalesces into it), and on the first record
	// of a replay snapshot, where it carries the ring's eviction count. It is
	// never set on a live-queue record: the live path counts gaps per unit in
	// pendingDropped instead.
	dropped int64
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
// immediately before this entry's text" -- on this entry's stream. It is
// produced by every place output can be lost -- eviction from a retention
// ring before a scrollback snapshot, a (spec, stream) live queue overflowing
// between two flushes, a log-file tailer skipping bytes past its read-rate
// cap (LogStore.gap), and the gateway's per-subscriber queue overflowing --
// and they are deliberately indistinguishable to the reader, because the
// reader only needs the one fact. A gap that renders as silence would be a lie
// about what the process printed, and silence is exactly what an operator is
// trying to interpret.
// Command is present only on an opening marker ("started"/"start_failed") and
// carries that generation's resolved, masked launch command (command.go). A
// reader renders it as part of the marker block it belongs to: it describes the
// process whose output follows, and only that one.
//
// Instance names the spec instance whose generation produced the entry (#340),
// on every text entry and every generation marker: 0 for the spec's own
// process -- omitted on the wire, which is also what an agent from before
// instances existed sends -- and k for instance k. The text of instance k keeps
// its "[#k] " line prefix (procLog.prefixLinesLocked), so a reader that ignores
// the field still reads one combined, attributable history; the field is what
// lets a reader show one instance's output alone. A gap cannot be attributed
// to an instance, because the bytes lost may have come from any of them:
// withDropped stamps it onto the first entry after it, which keeps naming the
// instance of its own text, and an entry made for a gap alone names none.
//
// Stream names the stream the entry belongs to (#430): omitted for process
// output -- which is also everything an agent from before log files sends --
// and a declared log file's name otherwise. Every entry carries it, a gap entry
// included: a gap is counted per stream, so the bytes it announces are missing
// from exactly that stream.
type LogEntry struct {
	PID          int              `json:"pid,omitempty"`
	At           time.Time        `json:"at"`
	Text         string           `json:"text,omitempty"`
	DroppedBytes int64            `json:"dropped_bytes,omitempty"`
	Event        string           `json:"event,omitempty"`
	ExitCode     int              `json:"exit_code,omitempty"`
	Command      *ResolvedCommand `json:"command,omitempty"`
	Instance     int              `json:"instance,omitempty"`
	Stream       string           `json:"stream,omitempty"`
}

// LogBatch is the payload of one agent->gateway runtime_log frame: the
// entries accumulated for ONE stream of ONE spec since the previous flush, or
// one CHUNK of that spec's history replay -- which covers every stream of the
// spec in one sequence, so a chunk may hold entries of several streams, each
// naming its own (LogEntry.Stream).
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

// specLog is one spec's retained history: one byte-bounded ring per stream,
// each spanning every generation of that spec, opening markers and their
// resolved commands included (those live in the process stream).
type specLog struct {
	streams   map[string]*streamRing
	lastWrite time.Time // the newest write on ANY stream; the eviction order
}

// streamRing is one stream's retained records. cap is fixed when the ring is
// created: the per-spec setting for the process stream, LogStore.fileCap for a
// log file (see NewLogStore).
type streamRing struct {
	recs    []logRecord
	bytes   int   // sum of every record's size(), i.e. text and commands PLUS overhead
	dropped int64 // bytes missing from the front: text evicted, plus gaps evicted with their record
	cap     int
}

// streamKey names one live-queue unit: a stream of a spec. Drain visits these
// round-robin, at most one batch each.
type streamKey struct{ spec, stream string }

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
// THE MEMORY BOUND, exactly. Retention is a hard BYTE bound: used, the sum of
// every ring of every spec INCLUDING per-record overhead, never exceeds total
// after any write. Each ring is first trimmed to its own cap (perSpec for the
// process stream, fileCap for a log file); then, while used is over total,
// whole UNWATCHED specs -- every stream together -- are dropped,
// least-recently-written first, never the spec just written and never a
// watched one. If that is not enough (every other spec is watched), the ring
// just written gives up its own oldest bytes, counted as its gap like any
// eviction -- so the bound holds whatever the watch set is. A spec with four
// log files holds up to perSpec + 4*fileCap, i.e. up to five times the
// per-spec size at the default, so fewer specs fit under the same total.
// NewLogStore guarantees one such spec always fits (see there).
//
// On top of retention, each WATCHED (spec, stream) unit can hold a live queue
// of at most logPendingBytes (64 KiB) -- five units for a spec with four
// files -- plus, while a history replay is in flight for a spec, a scrollback
// snapshot, which is a shallow copy that shares its strings with the retention
// rings and so adds only record headers unless the rings churn underneath it.
// A generation's opening marker also carries its masked launch command, capped
// at maxResolvedCommandBytes (16 KiB) and charged against the process ring's
// capacity like the text (logRecord.size), so it widens no bound. With the
// defaults (1 MiB / 16 MiB) and the one or two specs an operator actually
// watches at a time, that is 16 MiB steady-state and under 17 MiB while
// watching a spec without log files (each watched file adds at most 64 KiB of
// live queue).
//
// Every method is safe for concurrent use. Write in particular is called
// from os/exec's two copying goroutines per process, for every process at
// once; the single mutex covers both the retention append and the live
// queue, which is what lets Drain take a history snapshot and cut that queue
// as one indivisible step against a concurrent write -- and therefore what
// keeps the history from being delivered twice or torn.
type LogStore struct {
	mu sync.Mutex

	perSpec int
	// fileCap is the ring cap of every log-file stream (NewLogStore).
	fileCap int
	// total is the hard byte ceiling over every ring of every spec, and used
	// is what they hold right now: the sum of every ring's bytes, kept
	// incrementally by every append, trim and forget.
	total int
	used  int

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

	// The live queue, per (spec, stream) unit: a file at its read-rate cap
	// overflows its own queue, never the process output's.
	pending        map[streamKey][]logRecord
	pendingBytes   map[streamKey]int
	pendingDropped map[streamKey]int64

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
	// EMPTY history is a meaningful answer that still has to be delivered. A
	// replay is per SPEC: one record sequence covering every stream, each
	// stream's eviction gap carried on its own first record (specLog.snapshot).
	scrollback      map[string][]logRecord
	scrollbackReady map[string]bool

	nextGen uint64
	// rr and rrFile rotate Drain's starting point over the process units and
	// over the file units respectively, so the per-drain byte budget cannot
	// systematically starve whichever unit sorts last in either list.
	rr     int
	rrFile int

	now func() time.Time
}

// maxLogFileStreams is how many log-file rings NewLogStore sizes the total
// for: the spec field's own limit (log_files holds at most four entries).
const maxLogFileStreams = 4

// NewLogStore builds a store with an operator-chosen per-spec capacity and
// total ceiling. Both are clamped into a sane range rather than rejected: a
// misconfigured buffer size must never stop an agent from supervising model
// processes, which is its actual job.
//
// The process ring of every spec gets the full per-spec capacity. A log-file
// ring gets the same, unless one spec's five rings would not fit the total:
// then the file rings are clamped equally to what the total leaves after the
// process ring, min(perSpec, max(64 KiB, (total-perSpec)/4)). The total itself
// is raised to at least perSpec + 4*64 KiB -- just as it was always raised to
// at least one buffer -- so the 64 KiB floor holds and one spec with all its
// rings full always fits: perSpec + 4*fileCap <= total by construction.
// Capacity reports the effective numbers for the startup log line.
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
	if floor := perSpecBytes + maxLogFileStreams*minLogBufferBytes; totalBytes < floor {
		totalBytes = floor
	}
	fileCap := min(perSpecBytes, max(minLogBufferBytes, (totalBytes-perSpecBytes)/maxLogFileStreams))
	return &LogStore{
		perSpec:         perSpecBytes,
		fileCap:         fileCap,
		total:           totalBytes,
		logs:            make(map[string]*specLog),
		watching:        make(map[string]bool),
		watchEpoch:      make(map[string]uint64),
		pending:         make(map[streamKey][]logRecord),
		pendingBytes:    make(map[streamKey]int),
		pendingDropped:  make(map[streamKey]int64),
		scrollbackDue:   make(map[string]bool),
		scrollback:      make(map[string][]logRecord),
		scrollbackReady: make(map[string]bool),
		now:             time.Now,
	}
}

// Capacity reports the resolved per-spec ring and total byte ceilings (after
// clamping and raising), for the one startup log line that tells an operator
// what their setting actually became.
func (s *LogStore) Capacity() (perSpec, total int) {
	if s == nil {
		return 0, 0
	}
	return s.perSpec, s.total
}

// newProc opens a fresh generation for specID and returns the io.Writer that
// startProcess assigns to BOTH exec.Cmd.Stdout and .Stderr. The requirement
// says stdout and stderr both, and they deliberately share one writer so the
// interleaving an operator reads is the interleaving the process produced. It
// is newInstanceProc for instance 0: the spec's own process, whose output is
// recorded byte for byte.
func (s *LogStore) newProc(specID string) *procLog {
	return s.newInstanceProc(specID, 0)
}

// newInstanceProc opens a fresh generation for instance k of the spec specID
// (#340). Every instance of a spec writes into the ONE buffer of its spec --
// specID is always the BASE spec's ID, never an instance key -- so the spec's
// log view, the gateway's watch command (which names spec IDs) and Retain
// (which keeps spec IDs) all cover every instance with nothing per instance to
// track, a crashed burst instance's output stays readable after its row is
// retired, and a spec running many instances holds one buffer of the per-spec
// budget, not one per instance.
//
// Instance 0's output is recorded exactly as it was written. Each line a
// further instance writes is prefixed with "[#k] " (instanceLinePrefix), so the
// operator can tell the instances apart in the one interleaved history. The
// generation markers need no prefix: they are typed, and carry the pid. Every
// record of the generation, text and markers alike, also carries k itself
// (LogEntry.Instance), so a reader can tell the instances apart without parsing
// the prefix back out of the text.
func (s *LogStore) newInstanceProc(specID string, k int) *procLog {
	return s.newInstanceProcMasked(specID, k, nil)
}

// newInstanceProcMasked is newInstanceProc for a generation whose launch
// substituted secrets (#430 §8): every stream of the generation masks m's
// values before they are recorded (see LogStore.write). startProcess builds m
// from the generation's expandedSpec, the one place the plaintext exists; a
// nil m masks nothing.
func (s *LogStore) newInstanceProcMasked(specID string, k int, m *secretMasker) *procLog {
	if s == nil {
		return &procLog{}
	}
	s.mu.Lock()
	s.nextGen++
	gen := s.nextGen
	s.mu.Unlock()
	return &procLog{store: s, specID: specID, gen: gen, instance: k, prefix: instanceLinePrefix(k), masker: m}
}

// instanceLinePrefix is the text that opens every line instance k of a spec
// writes into the spec's buffer: none for instance 0, whose output is the
// spec's own exactly as before instances existed, and "[#k] " for k >= 1.
func instanceLinePrefix(k int) string {
	if k <= 0 {
		return ""
	}
	return "[#" + strconv.Itoa(k) + "] "
}

// write records b as p's generation's output on p's stream and, when the spec
// is watched, queues it for the next Drain. Never blocks on a reader and never
// fails: there is no failure mode for a bounded in-memory append, and a log
// write must never be able to stall the process it is reading from.
//
// The order is: mask the generation's own resolved secrets (#430 §8), then
// prefix a further instance's lines (prefixLinesLocked), then append and
// queue -- all under the one lock, so the carry, the prefix state and the
// order the chunks land in can never disagree. Masking comes first so no byte
// of a secret ever reaches the ring, the live queue, Tail (and so
// LastError.StderrTail) or anything built from them; the placeholder it
// leaves contains no newline, so the prefix step sees the same lines.
//
// The masker holds back only the bytes that could still begin a secret split
// across two writes (p.carry; at most its longest value minus one byte, plus
// up to three to keep a character whole); those bytes land with the next
// write or at flushLocked, so live delivery and Tail lag by at most them. A
// write that only grows the carry records nothing. Bytes held back before
// the generation's started marker land after it: a cosmetic reordering, as a
// split secret must never be flushed early.
//
// Concurrency: the carry is a single byte sequence in the order writes take
// the lock. os/exec hands a writer that is both Stdout and Stderr to a single
// copying goroutine over one pipe, so for the process stream the order is the
// child's own; were two goroutines ever to write, each write would still join
// the carry whole and in lock order -- no byte lost or duplicated -- and a
// secret split by the other writer's bytes would simply not be contiguous,
// and so not masked (best effort, like every other non-literal form).
func (s *LogStore) write(p *procLog, b []byte) {
	if s == nil || p.specID == "" || len(b) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.masker == nil {
		s.recordLocked(p, string(b))
		return
	}
	s.recordLocked(p, p.masker.mask(&p.carry, b, false))
}

// flushLocked records whatever p's masker still holds back (the end of a
// stream: nothing can complete a split value any more). The caller holds mu.
func (s *LogStore) flushLocked(p *procLog) {
	if p.carry.empty() {
		return
	}
	s.recordLocked(p, p.masker.mask(&p.carry, nil, true))
}

// recordLocked prefixes already-masked text and appends and queues it as one
// text record of p's stream. The caller holds mu.
func (s *LogStore) recordLocked(p *procLog, text string) {
	if text == "" {
		return
	}
	rec := logRecord{gen: p.gen, pid: int(p.pid.Load()), at: s.now(), text: p.prefixLinesLocked(text), instance: p.instance, stream: p.stream}
	s.appendLocked(p.specID, rec)
	s.queueLocked(streamKey{p.specID, p.stream}, rec)
}

// mark records a lifecycle marker (a generation boundary, or a file event) in
// the same history and live queue the output of p's stream flows through, so
// its position relative to the surrounding output is exact.
//
// cmd is non-nil only for an OPENING marker, and it is the masked launch
// command of the generation that marker opens (see ResolvedCommand). It is
// carried on the record, so it is retained for exactly as long as that record
// is and is delivered wherever that record is -- scrollback and live alike --
// with no separate bookkeeping.
func (s *LogStore) mark(p *procLog, pid int, event string, exitCode int, cmd *ResolvedCommand) {
	if s == nil || p.specID == "" {
		return
	}
	specID := p.specID
	rec := logRecord{gen: p.gen, pid: pid, at: s.now(), event: event, exitCode: exitCode, command: cmd, instance: p.instance, stream: p.stream}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(specID, rec)
	s.queueLocked(streamKey{specID, p.stream}, rec)
}

// gap counts n bytes of p's stream that were skipped rather than recorded --
// a log-file tailer's read-rate cap -- so the reader sees a counted gap, not
// silence. In the retained history the gap is a record at the position the
// bytes were skipped (the next text of the generation coalesces into it, so it
// is reported as "missing immediately before" that text, exactly where it
// happened); in a watched spec's live queue it is counted in that unit's
// pendingDropped, like a queue overflow.
func (s *LogStore) gap(p *procLog, n int64) {
	if s == nil || p.specID == "" || n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// No instance on the gap record: a gap names none (LogEntry's doc). The
	// generation's next text coalesces into it and brings its own.
	s.appendLocked(p.specID, logRecord{gen: p.gen, at: s.now(), stream: p.stream, dropped: n})
	if s.watching[p.specID] {
		s.pendingDropped[streamKey{p.specID, p.stream}] += n
	}
}

// logForLocked returns specID's history, creating it on first use. The total
// ceiling is enforced by appendLocked, after the bytes it is about land.
func (s *LogStore) logForLocked(specID string) *specLog {
	l := s.logs[specID]
	if l == nil {
		l = &specLog{streams: make(map[string]*streamRing)}
		s.logs[specID] = l
	}
	return l
}

// ringCap is the cap a new ring of stream gets.
func (s *LogStore) ringCap(stream string) int {
	if stream == "" {
		return s.perSpec
	}
	return s.fileCap
}

// appendLocked adds rec to its stream's ring of specID's history, coalescing
// consecutive same-generation output of that stream, evicting from the ring's
// front to stay within its cap, and then restoring the total ceiling.
func (s *LogStore) appendLocked(specID string, rec logRecord) {
	l := s.logForLocked(specID)
	l.lastWrite = rec.at
	r := l.streams[rec.stream]
	if r == nil {
		r = &streamRing{cap: s.ringCap(rec.stream)}
		l.streams[rec.stream] = r
	}
	before := r.bytes
	if !coalesceInto(r, rec) {
		r.recs = append(r.recs, rec)
		r.bytes += rec.size()
	}
	trimRing(r, r.cap)
	s.used += r.bytes - before
	s.enforceTotalLocked(specID, r)
}

// coalesceInto joins rec onto the ring's last record when both are plain output
// of the same stream and generation, and reports whether it did. A gap joins a
// gap-only record (the two are one longer gap); text joins anything that is not
// a marker, a gap record included -- which is what puts a gap immediately
// before the text that follows it.
func coalesceInto(r *streamRing, rec logRecord) bool {
	if rec.event != "" || len(r.recs) == 0 {
		return false
	}
	last := &r.recs[len(r.recs)-1]
	if last.event != "" || last.gen != rec.gen || last.stream != rec.stream || len(last.text) >= logCoalesceTarget {
		return false
	}
	if rec.dropped > 0 {
		if last.text != "" {
			return false
		}
		last.dropped += rec.dropped
		return true
	}
	if last.text == "" {
		// A gap-only record taking its first text: the entry is that text,
		// with the gap in front of it, so it takes the text's identity.
		last.pid, last.at, last.instance = rec.pid, rec.at, rec.instance
	}
	last.text += rec.text
	r.bytes += len(rec.text)
	return true
}

// trimRing drops a ring's oldest bytes until it is back within limit, counting
// exactly how much it discarded -- text, and any gap that went with its record
// -- so a later scrollback can report it instead of showing a silent gap.
func trimRing(r *streamRing, limit int) {
	for r.bytes > limit && len(r.recs) > 0 {
		excess := r.bytes - limit
		head := &r.recs[0]
		if len(head.text) > excess {
			// strings.Clone, not a bare reslice: a Go string slice keeps the
			// WHOLE backing array alive, which would leave the discarded
			// prefix -- potentially prompt-bearing -- reachable for as long
			// as the remainder lives. Same reasoning the predecessor of this
			// file recorded for its own reallocate-on-overflow.
			head.text = strings.Clone(head.text[excess:])
			r.bytes -= excess
			// The head's own gap was in front of the text just cut, so it is
			// now part of what is missing from the ring's front.
			r.dropped += int64(excess) + head.dropped
			head.dropped = 0
			return
		}
		r.dropped += int64(len(head.text)) + head.dropped
		r.bytes -= head.size()
		r.recs[0] = logRecord{} // release the string before dropping the slot
		r.recs = r.recs[1:]
	}
}

// enforceTotalLocked restores used <= total after a write to ring r of spec
// keep. First by dropping whole UNWATCHED specs other than keep, least-
// recently-written first (evictLocked). If every remaining spec is keep or
// watched, r -- the ring that just grew -- gives up its own oldest bytes by
// the excess, which is at most what r just gained, since used was within total
// before the write.
//
// Except the record r was just given, when it is a MARKER: a typed event has
// no text to count as a gap, so trimming it would make it vanish without a
// trace -- a file tab's first file_waiting, say. It is kept: r's older records
// give what they can, and the rest is taken from the oldest bytes elsewhere in
// the store (trimOldestLocked), counted as that ring's gap like any eviction.
// There is always enough: a marker is at most maxResolvedCommandBytes plus
// overhead, far below what the other rings hold when the total is full.
func (s *LogStore) enforceTotalLocked(keep string, r *streamRing) {
	if s.used <= s.total {
		return
	}
	s.evictLocked(keep)
	if s.used <= s.total {
		return
	}
	before := r.bytes
	n := len(r.recs)
	if n == 0 || r.recs[n-1].event == "" {
		trimRing(r, max(0, r.bytes-(s.used-s.total)))
		s.used += r.bytes - before
		return
	}
	// r's older records first, exactly as for text; the marker is set aside
	// for the trim and put back.
	last := r.recs[n-1]
	r.recs, r.bytes = r.recs[:n-1], r.bytes-last.size()
	trimRing(r, max(0, r.bytes-(s.used-s.total)))
	r.recs, r.bytes = append(r.recs, last), r.bytes+last.size()
	s.used += r.bytes - before
	s.trimOldestLocked(r)
}

// trimOldestLocked takes the excess over the total from the rings whose oldest
// record is oldest, store-wide -- protect's older records included, protect's
// newest record (the marker enforceTotalLocked keeps) never.
func (s *LogStore) trimOldestLocked(protect *streamRing) {
	for s.used > s.total {
		var victim *streamRing
		var victimAt time.Time
		for _, l := range s.logs {
			for _, r := range l.streams {
				if len(r.recs) == 0 || (r == protect && len(r.recs) == 1) {
					continue
				}
				if victim == nil || r.recs[0].at.Before(victimAt) {
					victim, victimAt = r, r.recs[0].at
				}
			}
		}
		if victim == nil {
			return
		}
		before := victim.bytes
		limit := max(0, victim.bytes-(s.used-s.total))
		if victim == protect {
			limit = max(limit, protect.recs[len(protect.recs)-1].size())
		}
		trimRing(victim, limit)
		s.used += victim.bytes - before
		if victim.bytes == before {
			return // cannot happen; a guard against looping forever
		}
	}
}

// evictLocked enforces the TOTAL ceiling by dropping whole spec histories --
// every stream together -- least-recently-written first, until used is back
// within total or no candidate is left. keep (the spec just written) and every
// watched spec are never the victim: a watched spec's history is what an
// operator is reading right now.
func (s *LogStore) evictLocked(keep string) {
	for s.used > s.total {
		victim := ""
		var victimAt time.Time
		for id, l := range s.logs {
			if id == keep || s.watching[id] {
				continue
			}
			if victim == "" || l.lastWrite.Before(victimAt) {
				victim, victimAt = id, l.lastWrite
			}
		}
		if victim == "" {
			return
		}
		s.forgetLocked(victim)
	}
}

// forgetLocked removes every trace of specID from the store, every stream's
// ring included, and returns their bytes to the total.
func (s *LogStore) forgetLocked(specID string) {
	if l := s.logs[specID]; l != nil {
		for _, r := range l.streams {
			s.used -= r.bytes
		}
	}
	delete(s.logs, specID)
	s.forgetStreamLocked(specID)
}

// forgetStreamLocked drops everything the STREAMING half holds for specID --
// the live queue of every one of its streams, any due or in-flight history
// replay, and the epoch that replay was taken for -- leaving the retention
// rings alone. It is what a spec leaving the watch set gets: there is no one
// left to deliver to, and holding any of it would be memory kept for nobody.
func (s *LogStore) forgetStreamLocked(specID string) {
	delete(s.watchEpoch, specID)
	for _, k := range s.unitsOfLocked(specID) {
		delete(s.pending, k)
		delete(s.pendingBytes, k)
		delete(s.pendingDropped, k)
	}
	delete(s.scrollbackDue, specID)
	delete(s.scrollback, specID)
	delete(s.scrollbackReady, specID)
}

// unitsOfLocked returns specID's live-queue units that hold anything (records
// or a counted drop), sorted by stream name -- the process stream first.
func (s *LogStore) unitsOfLocked(specID string) []streamKey {
	seen := map[streamKey]bool{}
	var out []streamKey
	add := func(k streamKey) {
		if k.spec == specID && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range s.pending {
		add(k)
	}
	for k := range s.pendingDropped {
		add(k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].stream < out[j].stream })
	return out
}

// queueLocked adds rec to unit k's live queue, but only while k's spec is
// watched -- this is the "no traffic on an unwatched fleet" half of the
// design. Past logPendingBytes the text is dropped and its length counted;
// a MARKER is never dropped, because losing "the process exited, code 1" is
// losing the very fact the operator is reading the stream to find.
func (s *LogStore) queueLocked(k streamKey, rec logRecord) {
	if !s.watching[k.spec] {
		return
	}
	if rec.event == "" && s.pendingBytes[k]+rec.size() > logPendingBytes {
		s.pendingDropped[k] += int64(len(rec.text))
		return
	}
	q := s.pending[k]
	if rec.event == "" && len(q) > 0 {
		last := &q[len(q)-1]
		if last.event == "" && last.gen == rec.gen && last.stream == rec.stream && len(last.text) < logCoalesceTarget {
			last.text += rec.text
			s.pendingBytes[k] += len(rec.text)
			return
		}
	}
	s.pending[k] = append(q, rec)
	s.pendingBytes[k] += rec.size()
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
// exists. The keys are SPEC IDs, for a replicable spec too: every instance of
// a spec writes into its spec's one buffer (newInstanceProc), so a spec's
// output -- a retired burst instance's included -- lives exactly as long as
// the spec does.
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
// it as at most one batch per (spec, stream) unit. It is called on a fixed
// flush cadence by the agent's run loop -- the SINGLE goroutine that writes to
// the gateway WebSocket -- so the batching here is also the rate limit.
// Exactly what it bounds, since the previous statement of it was false in two
// ways:
//
//   - at most ONE batch per (spec, stream) unit per interval; a spec's history
//     replay is ONE chunk per interval for the whole spec, every stream of it;
//   - at most logDrainBudget bytes per interval of LIVE output across all
//     units, visited round-robin;
//   - plus at most maxLogBatchBytes of HISTORY REPLAY per interval, fleet-wide,
//     in chunks of at most maxLogBatchBytes each. A replay is one-shot and only
//     ever follows a viewer arriving, so it has its own budget rather than
//     competing with live output for one -- but it is no longer unbounded,
//     which is what made a full retention buffer produce a single frame the
//     gateway could not read at all.
//
// THE UNIT IS (spec, stream), not spec (#430). Each unit has its own live
// queue of at most logPendingBytes, so the load-bearing relation
// logDrainBudget > logPendingBytes holds per unit exactly as it held per spec:
// a unit's whole queue always fits a fresh drain's budget, so deferring rather
// than tearing a batch can never strand one.
//
// PROCESS UNITS GO FIRST. Every drain visits the process unit of every watched
// spec (round-robin among themselves) before any file unit (round-robin among
// themselves), and the first unit of a round is served whatever its size. What
// that guarantees, exactly:
//
//   - files can never defer a process unit: the process units see the whole
//     budget before any file is taken, so a file at its read-rate cap costs
//     the live process view nothing -- not a delay and not a dropped byte. Only
//     other specs' process output competes with it, exactly as before files;
//   - a file unit is served in every drain in which the process units took at
//     most logDrainBudget - logPendingBytes (64 KiB), because its whole queue
//     then fits what is left; and when no process unit has anything, the first
//     file unit of the round is served whatever its size. A file unit waits
//     only while the fleet's process output alone sustains more than 256 KiB/s,
//     and what it cannot hold meanwhile is dropped and counted, never silent.
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
// other's copy. Nothing can land between the two. The cut covers every stream
// of the spec, one live batch per unit that held anything.
//
// While a replay is in flight for a spec, the live output of every one of its
// streams WAITS behind it -- torn history is a worse outcome than a delayed
// tail, and the wait is bounded (the default 1 MiB buffer replays in about two
// flush intervals per full ring) and honest (past the per-unit queue cap,
// bytes are dropped and counted).
//
// Returns nil when there is nothing to send, which on an unwatched fleet is
// always.
func (s *LogStore) Drain() []LogBatch {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	procUnits, fileUnits := s.watchedUnitsLocked()
	if len(procUnits)+len(fileUnits) == 0 {
		return nil
	}
	ordered := append(rotate(procUnits, &s.rr), rotate(fileUnits, &s.rrFile)...)

	budget := logDrainBudget
	replayBudget := maxLogBatchBytes
	// replayed marks a spec whose replay (or the cut that precedes it) has had
	// its turn this interval: its other units wait behind it.
	replayed := map[string]bool{}
	var out []LogBatch
	for _, k := range ordered {
		id := k.spec
		if replayed[id] {
			continue
		}
		if hadViewer, due := s.scrollbackDue[id]; due {
			if cut := s.startReplayLocked(id, hadViewer); len(cut) > 0 {
				// The queues held output an EXISTING viewer is still owed. It
				// goes out as live batches first; the replay begins next
				// interval.
				out = append(out, cut...)
				replayed[id] = true
				continue
			}
		}
		if s.scrollbackReady[id] {
			replayed[id] = true
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
		q := s.pending[k]
		dropped := s.pendingDropped[k]
		if len(q) == 0 && dropped == 0 {
			continue
		}
		if s.pendingBytes[k] > budget && budget < logDrainBudget {
			// Leave it queued for the next drain rather than tearing the
			// batch: the rotation above guarantees this unit goes first soon,
			// and the per-unit queue cap keeps the wait bounded (past it,
			// bytes are dropped and counted, never silently lost).
			//
			// The `budget < logDrainBudget` half is what makes that promise
			// unconditional: it means "something has already been taken this
			// round". The FIRST unit of a round is served whatever its size,
			// so a queue that somehow exceeded the full budget (marker records
			// are admitted past the per-unit cap, by design, so that a
			// process-exit boundary can never be the thing that gets dropped)
			// is drained rather than stuck forever.
			continue
		}
		entries, rest, _ := takeStreamChunk(id, k.stream, q, dropped, maxLogBatchBytes)
		budget -= s.pendingBytes[k]
		delete(s.pendingDropped, k)
		if rest == nil {
			delete(s.pending, k)
			delete(s.pendingBytes, k)
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
			s.pending[k] = rest
			s.pendingBytes[k] = kept
		}
		out = append(out, LogBatch{SpecID: id, Entries: entries})
	}
	return out
}

// watchedUnitsLocked lists the (spec, stream) units Drain visits, as two
// sorted lists: the process unit of every watched spec -- always, because that
// is where a due or in-flight replay is found even when nothing is queued --
// and every file unit of a watched spec whose live queue holds anything.
func (s *LogStore) watchedUnitsLocked() (procUnits, fileUnits []streamKey) {
	seen := make(map[streamKey]bool, len(s.watching))
	procUnits = make([]streamKey, 0, len(s.watching))
	add := func(k streamKey) {
		if !s.watching[k.spec] || seen[k] {
			return
		}
		seen[k] = true
		if k.stream == "" {
			procUnits = append(procUnits, k)
		} else {
			fileUnits = append(fileUnits, k)
		}
	}
	for id := range s.watching {
		add(streamKey{spec: id})
	}
	for k := range s.pending {
		add(k)
	}
	for k := range s.pendingDropped {
		add(k)
	}
	sortUnits(procUnits)
	sortUnits(fileUnits)
	return procUnits, fileUnits
}

// sortUnits orders units by spec, then stream.
func sortUnits(u []streamKey) {
	sort.Slice(u, func(i, j int) bool {
		if u[i].spec != u[j].spec {
			return u[i].spec < u[j].spec
		}
		return u[i].stream < u[j].stream
	})
}

// rotate returns units starting at *rr (wrapped into range) and advances it, so
// successive drains start one unit further on.
func rotate(units []streamKey, rr *int) []streamKey {
	if len(units) == 0 {
		return nil
	}
	if *rr >= len(units) {
		*rr = 0
	}
	out := append(append([]streamKey(nil), units[*rr:]...), units[:*rr]...)
	*rr++
	return out
}

// startReplayLocked snapshots specID's retained history and cuts the live queue
// of every one of its streams in one step -- see Drain for why those two must
// not be separable.
//
// What becomes of the cut depends on whether anyone was ALREADY watching this
// spec. Everything the queues hold is also in the snapshot just taken (write
// appends to both, under this lock), so:
//
//   - a FIRST subscribe discards it: there is no viewer it belongs to, and the
//     replay carries every one of those bytes as history;
//   - a RE-snapshot, for a second viewer arriving, emits it as ordinary live
//     batches, one per unit that held anything, returned here so the caller
//     sends them before the replay begins. That is output the viewer already
//     watching is still owed, and it must neither be dropped (a silent gap for
//     them) nor left queued behind the replay (a duplicate of the replay's
//     tail for the viewer arriving).
func (s *LogStore) startReplayLocked(specID string, hadViewer bool) []LogBatch {
	delete(s.scrollbackDue, specID)
	if l := s.logs[specID]; l != nil {
		s.scrollback[specID] = l.snapshot()
	}
	s.scrollbackReady[specID] = true

	var out []LogBatch
	for _, k := range s.unitsOfLocked(specID) {
		q := s.pending[k]
		dropped := s.pendingDropped[k]
		delete(s.pending, k)
		delete(s.pendingBytes, k)
		delete(s.pendingDropped, k)
		if !hadViewer || (len(q) == 0 && dropped == 0) {
			continue
		}
		// Chunked like any other batch; a remainder is dropped rather than
		// requeued because the snapshot just taken contains every one of these
		// records already, so the arriving viewer loses nothing and the
		// watching one sees the same gap it would have seen from a full queue
		// -- counted, not silent.
		entries, rest, _ := takeStreamChunk(specID, k.stream, q, dropped, maxLogBatchBytes)
		if len(rest) > 0 {
			lost := int64(0)
			for _, r := range rest {
				lost += int64(len(r.text))
			}
			s.pendingDropped[k] = lost
		}
		out = append(out, LogBatch{SpecID: specID, Entries: entries})
	}
	return out
}

// snapshot is the record sequence a replay of l delivers: the process stream
// first, then every file stream by name, each in its own order. It is a
// shallow copy: strings are immutable and trimRing replaces rather than
// mutates them, so this shares the text with the retention rings instead of
// duplicating megabytes.
//
// Each ring's eviction count is stamped onto that ring's FIRST record -- it
// means "bytes of this stream missing immediately before this entry", and only
// the head of a stream has its evicted bytes in front of it -- or, for a ring
// left with no records at all, onto a gap-only record of its own. Either way
// it appears exactly once in the sequence and names its stream, which is what
// keeps a replay's gaps on the first chunk that reaches each stream and never
// repeated.
func (l *specLog) snapshot() []logRecord {
	names := make([]string, 0, len(l.streams))
	n := 0
	for name, r := range l.streams {
		names = append(names, name)
		n += len(r.recs) + 1
	}
	sort.Strings(names) // "" -- the process stream -- sorts first
	out := make([]logRecord, 0, n)
	for _, name := range names {
		r := l.streams[name]
		start := len(out)
		out = append(out, r.recs...)
		if r.dropped <= 0 {
			continue
		}
		if len(out) > start {
			out[start].dropped += r.dropped
		} else {
			out = append(out, logRecord{stream: name, dropped: r.dropped})
		}
	}
	return out
}

// replayChunkLocked emits the next chunk of specID's in-flight history replay,
// flagging More while any of it is left, and reports the wire bytes it used
// against the caller's fleet-wide replay budget. Every chunk carries Scrollback:
// it is the gateway's routing key for "this is history, for whoever asked for
// it". The chunk may span streams; every entry names its own.
func (s *LogStore) replayChunkLocked(specID string) (LogBatch, int) {
	// No batch-level drop: each stream's eviction count rides on that
	// stream's first record (snapshot), so it belongs to whichever chunk
	// carries that record -- once, and never a later chunk.
	entries, rest, used := takeStreamChunk(specID, "", s.scrollback[specID], 0, maxLogBatchBytes)
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
	return LogEntry{
		PID: r.pid, At: r.at, Text: r.text, DroppedBytes: r.dropped, Event: r.event,
		ExitCode: r.exitCode, Command: r.command, Instance: r.instance, Stream: r.stream,
	}
}

// withDropped attaches dropped (bytes of stream lost immediately BEFORE the
// first entry) to that first entry, which in a live batch is always of the same
// stream. A non-zero drop with no entries still produces one, naming the
// stream: the loss is the whole message, and swallowing it would turn a gap
// back into silence.
func withDropped(out []LogEntry, stream string, dropped int64) []LogEntry {
	if dropped <= 0 {
		return out
	}
	if len(out) == 0 {
		return []LogEntry{{DroppedBytes: dropped, Stream: stream}}
	}
	out[0].DroppedBytes += dropped
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
	return takeStreamChunk(specID, "", recs, dropped, limit)
}

// takeStreamChunk is takeChunk for a batch whose leading drop (if any) belongs
// to stream: the gap-only entry it may synthesize names that stream. The
// records themselves may be of any stream (a replay spans them all); each
// entry names its own.
func takeStreamChunk(specID, stream string, recs []logRecord, dropped int64, limit int) (entries []LogEntry, rest []logRecord, used int) {
	fixed := logBatchWireOverhead + jsonStringMaxBytes(specID)
	if dropped > 0 {
		// withDropped synthesizes an entry when there is nothing to stamp the
		// loss onto, so the room for one has to be reserved before the loop.
		fixed += logEntryWireOverhead + streamWireMax(stream)
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
	return withDropped(entries, stream, dropped), rest, limit - budget
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
	// The head is charged entryWireMax by the caller, so everything in it but
	// the text -- the envelope, the stream field, the two quotes -- is
	// reserved before the text is sized.
	room := budget - logEntryWireOverhead - streamWireMax(r.stream) - 2
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
	n = outsideLinePrefix(r, n)
	if n == 0 {
		return logRecord{}, logRecord{}, false
	}
	head, tail = r, r
	head.text, tail.text = r.text[:n], r.text[n:]
	// A gap is in front of the record's text, i.e. in front of the head; the
	// tail continues the text directly and has nothing missing before it.
	tail.dropped = 0
	return head, tail, true
}

// outsideLinePrefix moves a cut at n in r's text back to the start of its line
// when n falls inside the "[#k] " prefix the agent put there for instance k.
// A reader that shows one instance on its own removes that prefix from the
// start of each line, and it can only recognise the prefix whole: cut through
// it, the head's last line ends in a fragment like "[#2" and the tail opens
// with "] ", a line continued mid-way, so neither half is removed and the
// fragment shows up in that view. Moving the cut to the line start keeps the
// prefix whole in the tail. The cut only ever moves backwards, so the head
// still fits the budget; n == 0 then means nothing of r fits before the
// prefix, which defers r to the next chunk like any record too big for the
// room left. (A record cannot be alone in a chunk with a budget that small:
// alone, it has nearly the whole frame.)
func outsideLinePrefix(r logRecord, n int) int {
	prefix := instanceLinePrefix(r.instance)
	if prefix == "" || n <= 0 || n >= len(r.text) {
		return n
	}
	start := strings.LastIndexByte(r.text[:n], '\n') + 1
	if n-start < len(prefix) && strings.HasPrefix(r.text[start:], prefix) {
		return start
	}
	return n
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
	return n + streamWireMax(r.stream) + commandWireMax(r.command)
}

// streamWireMax is an upper bound on what an entry's "stream" field costs:
// nothing when it is the process stream (omitted), else the key, its
// punctuation and the measured name. Measured rather than folded into
// logEntryWireOverhead, so the bound does not depend on the name-length rule
// being enforced somewhere else.
func streamWireMax(stream string) int {
	if stream == "" {
		return 0
	}
	return len(`,"stream":`) + jsonStringMaxBytes(stream)
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

// tail returns the last n bytes of PROCESS OUTPUT written by generation gen,
// ignoring markers, every other generation and every log-file stream -- the
// exact semantics LastError.StderrTail had when the buffer still belonged to
// one process. The process stream is named explicitly: file text of the same
// generation must never leak into stderr_tail.
func (s *LogStore) tail(specID string, gen uint64, n int) string {
	return s.tailStream(specID, "", gen, n)
}

// tailStream returns the last n bytes of text generation gen wrote on stream
// of specID, ignoring markers and every other generation: tail for any
// stream, which is what a log file's entry in last_error is cut from.
func (s *LogStore) tailStream(specID, stream string, gen uint64, n int) string {
	if s == nil || n <= 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.logs[specID]
	if l == nil || l.streams[stream] == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range l.streams[stream].recs {
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
// Write may be called concurrently from os/exec's copying goroutines, which is
// why every mutation goes through LogStore's mutex. The pid is an atomic
// because it is stored by the owner goroutine immediately after cmd.Start
// while a copying goroutine may already be copying: output written in that
// sliver carries pid 0, and the started marker that follows carries the real
// one.
//
// specID is always the BASE spec's ID (#340): a further instance of a
// replicable spec writes into its spec's buffer, every line opened by prefix
// ("[#k] "; empty for instance 0, whose bytes pass through untouched), and
// instance is that k, recorded on every record the generation produces.
// midLine is whether the generation's last write ended inside a line, so the
// next byte continues that line rather than opening a new one; it is guarded
// by the store's mutex like everything else a write touches.
//
// stream is the stream this writer records on (#430): "" for the process
// output exec.Cmd is handed, a declared log file's name for a sibling made by
// ForStream. Each sibling is its own procLog, so it has its own line state
// (midLine), and everything it writes goes through the same LogStore.write as
// the process output.
//
// masker is the generation's secret masker (#430 §8; nil when its launch
// substituted no maskable secret), shared by every sibling; carry is THIS
// stream's masking state (see LogStore.write), guarded by the store's mutex
// like midLine, because a split secret can only be completed by the same
// stream's next bytes.
type procLog struct {
	store    *LogStore
	specID   string
	gen      uint64
	pid      atomic.Int64
	instance int
	prefix   string
	midLine  bool
	stream   string
	masker   *secretMasker
	carry    maskCarry
}

// ForStream returns a sibling of p that records on the stream name: the same
// store, spec, generation, instance and line prefix, so a file's text is
// attributed exactly like the process output of the same generation, but with
// its own line state, its own ring, its own live queue and its own gap count.
// It shares p's secret masker, with a carry of its own. It carries no
// pid -- a file is not the process's pipe -- so its text and markers have pid
// 0. The log-file tailer of one generation and declared file writes through
// it.
func (p *procLog) ForStream(name string) *procLog {
	return &procLog{store: p.store, specID: p.specID, gen: p.gen, instance: p.instance, prefix: p.prefix, stream: name, masker: p.masker}
}

// Mark records the typed event (one of the logEventFile* constants) on p's
// stream, at its exact position among that stream's text: no pid, no command,
// no exit code. The portal owns the wording.
func (p *procLog) Mark(event string) {
	if p.store != nil {
		p.store.mark(p, 0, event, 0, nil)
	}
}

// Write implements io.Writer. It never returns an error: there is no failure
// mode for a bounded in-memory append, and reporting one would make os/exec
// stop copying the stream -- silencing the process the operator is reading.
func (p *procLog) Write(b []byte) (int, error) {
	if p.store != nil {
		p.store.write(p, b)
	}
	return len(b), nil
}

// Flush records the bytes p's secret masker still holds back, masked as the
// end of the stream (see LogStore.write). Call it where p's stream ends: the
// process's exit (Exited does it first, so its marker follows every byte), a
// log-file tailer's stop, and before a discontinuity in a file (rotation,
// truncation, a counted gap), where the bytes before and after can no longer
// form one value. A no-op without a masker or carry.
func (p *procLog) Flush() {
	if p.store == nil || p.specID == "" {
		return
	}
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	p.store.flushLocked(p)
}

// DiscardCarry drops what p's secret masker holds back, instead of resolving
// it as Flush does, and returns how many bytes of the stream that removes
// from the record. It is for a cut the reader of p's stream makes itself in
// the middle of the text (a log-file tailer's line cut, or a skip past its
// read-rate cap): the bytes after such a cut are not the continuation of the
// held-back ones, so resolving them as the end of the stream would write a
// secret's first part in clear. The count is what the caller reports as a
// gap. Bytes inside a span that is still open are already represented by the
// placeholder written when it opened, so they are not counted. A no-op
// without a masker or carry.
func (p *procLog) DiscardCarry() int {
	if p.store == nil || p.specID == "" {
		return 0
	}
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	n := len(p.carry.pending)
	if p.carry.open {
		n -= min(p.carry.end, n)
	}
	p.carry = maskCarry{}
	return n
}

// prefixLinesLocked returns b as the text to record: b itself for instance 0,
// and for a further instance b with p.prefix opening every line that starts in
// it. A line starts at the generation's first byte and after every '\n'; the
// prefix is written when the line's first byte arrives, not when the newline
// before it does, so output that ends with a newline leaves no dangling prefix
// and a line split across two writes is prefixed once. The caller holds the
// store's mutex.
func (p *procLog) prefixLinesLocked(b string) string {
	if p.prefix == "" {
		return b
	}
	var out strings.Builder
	out.Grow(len(b) + len(p.prefix)*(1+strings.Count(b, "\n")))
	for b != "" {
		if !p.midLine {
			out.WriteString(p.prefix)
		}
		i := strings.IndexByte(b, '\n')
		if i < 0 {
			out.WriteString(b)
			p.midLine = true
			break
		}
		out.WriteString(b[:i+1])
		b = b[i+1:]
		p.midLine = false
	}
	return out.String()
}

// Started records the generation boundary that opens this process's output,
// together with the resolved command it was launched with (already masked --
// see command.go). The two are recorded in one call because they are one fact:
// "this pid, running this command, from here on".
func (p *procLog) Started(pid int, cmd ResolvedCommand) {
	p.pid.Store(int64(pid))
	if p.store != nil {
		p.store.mark(p, pid, logEventStarted, 0, &cmd)
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
		p.store.mark(p, 0, logEventStartFailed, 0, &cmd)
	}
}

// Exited records the generation boundary that closes it. Together with
// Started these are what makes a crash loop READABLE: the appended history of
// several attempts, with the exit code of each one in its right place.
//
// It flushes the masker's carry first (Flush), so the marker lands after every
// byte the generation wrote, whichever caller closes it.
func (p *procLog) Exited(exitCode int) {
	if p.store != nil {
		p.Flush()
		p.store.mark(p, int(p.pid.Load()), logEventExited, exitCode, nil)
	}
}

// Tail returns the last n bytes THIS generation wrote on p's stream (see
// LogStore.tailStream) -- for the process writer, the bounded snapshot that
// becomes LastError.StderrTail. Its output must not be persisted anywhere
// durable; the gateway keeps it in volatile memory only, exactly like the rest
// of this path.
func (p *procLog) Tail(n int) string {
	if p.store == nil {
		return ""
	}
	return p.store.tailStream(p.specID, p.stream, p.gen, n)
}
