// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
)

// This file is the gateway's half of live managed-process log streaming (T3):
// it relays what an agent reports about a model process's stdout+stderr to
// whichever portal operators currently have a log view open, and it is what
// tells the agent to start and stop producing that output in the first place.
//
// THE SAFETY RULE. Everything passing through here is managed-process output,
// which for an inference server routinely contains fragments of prompt text.
// It is therefore held in volatile memory, for exactly as long as it takes to
// hand to a connected subscriber, and NEVER written to the database, never
// written to disk, and never put into a log line -- not even truncated, not
// even at Debug. That is the same rule the agent's own internal/runtime/logs.go
// states for the producing half, and the reason this feature needs no
// data-retention decision at all: there is nothing retained to decide about.
// The nearest precedent in this package is runtimeStatusRegistry, which keeps
// LastError.StderrTail in RAM only for the same reason.
//
// The same rule covers the RESOLVED COMMAND an opening marker can carry
// (RuntimeLogCommandDTO). It is not process output, so it cannot contain prompt
// text -- but it is a resolved argv and environment, which is closer to user
// data than status ever was, and it has no more reason to be persisted than
// output does. Volatile, relayed, forgotten: one rule for the whole frame.
//
// THE SUBSCRIPTION MODEL. Streaming happens only while someone is watching. A
// portal log view subscribes here; if it is the first subscriber for that
// (server, spec) the registry tells the agent, over its open WebSocket, the
// new full set of specs it should stream; when the last subscriber leaves, the
// agent is told the smaller set and stops. An unwatched fleet therefore
// produces no log traffic at all, which is what makes the feature affordable
// and what keeps a continuous flow of potentially prompt-bearing text off the
// wire when nobody has asked for it.

const (
	// runtimeLogSubBuffer is one subscriber's queue depth, in BATCHES. Each
	// batch is one agent flush window (250 ms) for one spec, so this is about
	// four seconds of slack for a browser that has briefly stopped reading --
	// a backgrounded tab, a slow render. Past it, batches are dropped and
	// their bytes COUNTED, and the count rides on the next batch that does
	// get through (see runtimeLogSub.take): a gap the reader can see, never
	// silence.
	runtimeLogSubBuffer = 16
	// runtimeLogMaxEntries bounds how many entries one inbound frame may
	// carry into the fan-out. The WebSocket read limit already bounds a frame
	// at 1 MiB, so this is a guard on entry COUNT (a frame of a million empty
	// entries) rather than on volume.
	runtimeLogMaxEntries = 4096
	// runtimeLogMaxSpecIDLen clamps a reported spec id. Ids the gateway
	// issues are ULIDs; anything longer is a malformed or hostile agent, and
	// the value is used as a fan-out map key.
	//
	// It bounds the id in BOTH directions, and the two do different things with
	// it. On ingest (ingestRuntimeLog) an over-long id is CLAMPED, because the
	// value is only a fan-out key there and a truncated one simply matches
	// nothing. On subscribe (handleRuntimeLogEvents) it is REJECTED, because
	// there the id is also a value SHIPPED TO THE AGENT inside the outbound
	// runtime_log_config command -- and because clamping the two ends
	// differently would produce a subscription that can never match a published
	// batch and is therefore guaranteed to sit empty forever, which is the
	// outcome this feature exists to eliminate.
	runtimeLogMaxSpecIDLen = 128
	// runtimeLogMaxWatchedSpecs bounds how many DISTINCT specs of one server may
	// have an open log view at once. It mirrors the agent's own maxWatchedSpecs
	// (server-agent/internal/runtime/driver.go), which silently truncates the
	// excess -- so without this the gateway would happily accept subscriptions
	// the agent will never honour and leave those windows empty with no
	// explanation. Every watched id is also marshaled into the outbound command,
	// so this and runtimeLogMaxSpecIDLen together are what keep that frame
	// (~8 KiB at the ceiling) far below the agent's own 1 MiB read limit; before
	// them, one authorized caller holding a pair of GETs with ~600 KiB of
	// spec_id each produced a frame the agent could not read, on a connection
	// the gateway then re-sent it on at every reconnect.
	//
	// Far above any reachable operator behaviour -- a human watches one or two
	// processes -- so it is a guard, not a policy an operator can hit.
	runtimeLogMaxWatchedSpecs = 64
	// runtimeLogSubMarkerCarry bounds the boundary markers held for a subscriber
	// whose queue is full (see runtimeLogSub.deliver). Four seconds of queue
	// plus 256 markers is well over a hundred process generations; a reader
	// further behind than that is not slow, it is gone, and deliver stops
	// dropping markers and resyncs it instead.
	runtimeLogSubMarkerCarry = 256
)

// runtimeLogsFeature is the negotiated name for live managed-process log
// streaming. Both halves of the negotiation live in this package -- the
// gateway's own declaration (gatewayAgentFeatures, agent_features.go) and the
// check against what an agent declared (Server.runtimeLogState) -- so unlike
// "runtime_manager", which is also spelled out in the agent module, this one
// is a named constant used by both, and the two cannot drift.
const runtimeLogsFeature = "runtime_logs"

// Live-stream states reported to a portal log view. The distinction is the
// whole point of feature negotiation here: three different silences that need
// three different things from the operator.
const (
	// runtimeLogStateStreaming: the agent is connected and understands the
	// command. Silence from here on genuinely means the process is quiet.
	runtimeLogStateStreaming = "streaming"
	// runtimeLogStateUnsupported: the agent is connected but does not declare
	// runtime_logs, so it will never answer the request. Without this state
	// the operator watches an empty window forever, unable to tell it from a
	// model that prints nothing -- the defect class this whole negotiation
	// exists to prevent. The portal says "update the agent".
	runtimeLogStateUnsupported = "unsupported"
	// runtimeLogStateOffline: no open agent WebSocket for this server, so
	// there is no gateway->agent direction to ask over at all. Reached by a
	// stopped agent, an unreachable one, and -- permanently -- by an agent
	// configured with the POST transport.
	runtimeLogStateOffline = "offline"
)

// RuntimeLogEntryDTO is one unit of managed-process output as it reaches a
// portal subscriber. Mirrors the agent's runtime.LogEntry field-for-field
// (server-agent/internal/runtime/logs.go).
//
// DroppedBytes is the overflow marker and means one thing everywhere: "N bytes
// the process printed are missing immediately before this entry's text". The
// agent produces it for output evicted from its retention buffer or lost to
// its own send queue; this gateway produces it for batches lost to a slow
// portal subscriber. The reader does not need to tell those apart -- it needs
// to know the gap is there, because a gap rendered as silence is a lie about
// what the process printed, and silence is exactly what the operator is trying
// to interpret.
//
// Event is a CLOSED set ("started"/"exited"/"start_failed"), allow-listed on
// ingest. The portal renders these as localized boundary markers, so an agent
// must not be able to put free text where an operator reads a gateway-authored
// sentence.
//
// Command rides on an OPENING marker ("started"/"start_failed") and carries that
// generation's resolved launch command. It is allow-listed the same way the event
// kind is -- stripped from any entry that is not an opening marker -- so a
// command can never appear attached to a line of process output, where it would
// describe nothing and could only mislead.
type RuntimeLogEntryDTO struct {
	PID          int                   `json:"pid,omitempty"`
	At           string                `json:"at,omitempty"`
	Text         string                `json:"text,omitempty"`
	DroppedBytes int64                 `json:"dropped_bytes,omitempty"`
	Event        string                `json:"event,omitempty"`
	ExitCode     int                   `json:"exit_code,omitempty"`
	Command      *RuntimeLogCommandDTO `json:"command,omitempty"`
}

// RuntimeLogCommandDTO is the RESOLVED launch command of ONE generation: what
// the agent actually exec'd, after every
// ${PORT}/${MODEL}/${HOST_GPU_IDS}/${AGENT_ENV:NAME} placeholder was resolved.
// Mirrors the agent's runtime.ResolvedCommand field-for-field
// (server-agent/internal/runtime/command.go), which is also where the masking
// rule and its limits are documented.
//
// It arrives as a TYPED FIELD on that generation's opening marker entry, never
// as text in the stream, and it carries no pid of its own -- the marker it hangs
// on has one. That placement is what makes attribution structural: the marker IS
// the generation boundary, so the command cannot end up describing a different
// attempt than the output around it, and a crash loop shows each attempt's own.
// Text would have been forgeable by a model server printing a convincing line;
// a typed field cannot be.
//
// THE GATEWAY DOES NOT RE-MASK THIS, and that is a decision rather than an
// omission. Masking a resolved command correctly requires knowing which BYTES
// of which string came from which placeholder, and only the agent -- the party
// that performed the substitution -- can know that. The gateway's options
// would be to mask everything (which would destroy the field's entire reason
// for existing: `--port 54331`, `--ctx-size 262144` and
// `CUDA_VISIBLE_DEVICES=2,3` are what an operator opened the panel to see) or
// to guess by pattern, which is worse than not masking because it looks like a
// guarantee. So the gateway clamps sizes -- it never trusts an agent's
// lengths or counts -- and relays the rest verbatim.
//
// Masked and EnvRedacted are the agent's two WITHHOLDING REASONS, one flag
// each, relayed verbatim like everything else here. They are independent and
// both can be set on the same command:
//
//   - Masked: at least one value was replaced by its own ${AGENT_ENV:NAME}
//     placeholder, which names a variable on the AI server that the operator
//     can go and check.
//   - EnvRedacted: the agent takes its specs from a local file, so at least
//     one spec-supplied env VALUE was withheld in full (key intact) -- the same
//     line the upward report draws around a document the gateway does not own.
//     There is no placeholder and nothing to look up.
//
// One flag for two reasons would leave the portal unable to say which mask an
// operator is looking at, and it renders a different sentence for each.
//
// Truncated says entries are missing -- set by the agent when the
// command exceeded its own cap, and by sanitizeRuntimeLogCommand when it
// exceeded the gateway's. Args and Env are agent/operator-authored strings:
// render them as text, never as HTML.
type RuntimeLogCommandDTO struct {
	Binary      string   `json:"binary,omitempty"`
	Args        []string `json:"args,omitempty"`
	WorkDir     string   `json:"work_dir,omitempty"`
	Env         []string `json:"env,omitempty"`
	Masked      bool     `json:"masked,omitempty"`
	EnvRedacted bool     `json:"env_redacted,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
}

// RuntimeLogBatchDTO is one SSE `log` frame: the entries an agent flushed for
// one spec.
//
// Scrollback marks the one-shot history replay a subscribe produces. It is
// load-bearing: the portal RESETS its view on it (an agent reconnect delivers
// a fresh scrollback, and appending would duplicate the history), and an
// EMPTY scrollback batch is itself an answer -- "the agent's retained buffer
// is empty", which is what an agent restart leaves behind -- as distinct from
// "nothing has arrived yet".
//
// More is INBOUND ONLY and never reaches the portal. A retained history can be
// larger than one WebSocket frame, so the agent sends a replay as a sequence of
// batches, every one flagged Scrollback and every one but the last also flagged
// More (server-agent/internal/runtime/logs.go's LogBatch). runtimeLogSub.deliver
// translates that sequence into what the portal's contract has always said:
// Scrollback set on exactly the first batch each subscriber receives, cleared on
// the rest, and More cleared throughout. A portal therefore needs no notion of
// chunking, and one that predates it renders a chunked replay correctly.
//
// The resolved launch command travels inside the entries, on each generation's
// opening marker (RuntimeLogEntryDTO.Command), not as a field here: a batch is a
// time-slice that can span two generations, and a command belongs to exactly
// one. It rides this frame rather than a channel of its own because it describes
// the very process whose output follows, it is wanted only while a log view is
// open, and this frame's subscription -- and therefore its authorization
// boundary -- already exists. That boundary is not laxer for carrying argv:
// handleRuntimeLogEvents checks server ownership/admin-group before the first
// stream byte, exactly as it does for output.
type RuntimeLogBatchDTO struct {
	SpecID     string               `json:"spec_id"`
	Scrollback bool                 `json:"scrollback,omitempty"`
	More       bool                 `json:"scrollback_more,omitempty"`
	Entries    []RuntimeLogEntryDTO `json:"entries"`
}

// runtimeLogSub is one open portal log view: a bounded queue, the count of
// bytes dropped because that queue was full, the boundary markers rescued from
// those dropped batches, and how much of its history replay it has had. The
// counters are separate from the queue on purpose -- a full queue must not need
// a slot to record that it was full.
type runtimeLogSub struct {
	ch      chan RuntimeLogBatchDTO
	dropped atomic.Int64

	// resync is closed when this subscriber has fallen so far behind that the
	// next thing lost would be a boundary marker. Ending the stream makes the
	// browser's EventSource reconnect, and a reconnect now genuinely re-snapshots
	// (see subscribe), so the operator gets a complete history back instead of a
	// silently incomplete one. Closed at most once, by resyncLocked.
	resync chan struct{}

	mu sync.Mutex
	// carry holds markers (and the commands riding them) taken out of batches
	// that did not fit the queue, to be delivered in front of the next batch
	// that does. queueLocked in the agent protects exactly this property one
	// layer down -- "a MARKER is never dropped, because losing 'the process
	// exited, code 1' is losing the very fact the operator is reading the stream
	// to find" -- and before this the relay did not preserve it.
	carry []RuntimeLogEntryDTO
	// sbStarted: this subscriber has been given the first chunk of a replay, so
	// the next chunk must APPEND rather than reset. sbDone: it has the whole
	// history, so a replay produced for a viewer arriving later must not be
	// handed to it -- not disturbing the viewer already watching is the entire
	// constraint on re-snapshotting.
	sbStarted bool
	sbDone    bool
	closed    bool
}

// deliver hands one agent batch to this subscriber, or decides it is not for
// this subscriber at all. It is the whole per-viewer half of the relay:
//
//   - A SCROLLBACK batch (any chunk of a history replay) goes only to a
//     subscriber that has not completed one. The first chunk it receives keeps
//     scrollback=true, which is the portal's RESET signal; every later chunk has
//     it cleared, so a chunked replay renders exactly like the single batch a
//     small history still produces. More never travels portal-ward -- chunking
//     is an agent/gateway concern.
//   - A LIVE batch goes to everyone, unchanged.
//   - A batch that does not fit the queue has its TEXT dropped and counted, and
//     its markers kept (see carry).
//
// The scrollback state is committed only after the send SUCCEEDS: a reset that
// never reached the reader must not be recorded as one, or the reader would
// append a history it never reset for.
func (s *runtimeLogSub) deliver(batch RuntimeLogBatchDTO) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	replay, more := batch.Scrollback, batch.More
	batch.More = false
	if replay {
		if s.sbDone {
			return // it already has a history; leave it undisturbed
		}
		batch.Scrollback = !s.sbStarted
	}
	// Stamp at ENQUEUE time, not at read time: the rescued markers came from a
	// batch published after everything already queued, so this is the one
	// position where they are in order.
	dropped := s.dropped.Load()
	select {
	case s.ch <- s.stampedLocked(batch, dropped):
		s.carry = nil
		s.dropped.Add(-dropped)
		if replay {
			s.sbStarted = true
			s.sbDone = !more
		}
	default:
		// Nothing was consumed: the batch that would have carried them did not
		// fit either, so they stay owed.
		s.dropped.Add(batchTextBytes(batch))
		s.carryLocked(batch.Entries)
	}
}

// stampedLocked returns batch with the rescued markers in front of its entries
// and dropped accounted on the first of them. It COPIES rather than mutating:
// the batch value is shared by every subscriber of this spec (publish fans out
// one snapshot) and each has its own losses, so mutating in place would give one
// subscriber's gap to all of them. The per-entry Command pointers inside are
// shared and need no deep copy -- once sanitizeRuntimeLogCommand has run on
// ingest, nothing writes them again.
func (s *runtimeLogSub) stampedLocked(batch RuntimeLogBatchDTO, dropped int64) RuntimeLogBatchDTO {
	if dropped == 0 && len(s.carry) == 0 {
		return batch
	}
	entries := make([]RuntimeLogEntryDTO, 0, len(s.carry)+len(batch.Entries)+1)
	entries = append(entries, s.carry...)
	entries = append(entries, batch.Entries...)
	if dropped > 0 {
		if len(entries) == 0 {
			entries = append(entries, RuntimeLogEntryDTO{})
		}
		entries[0].DroppedBytes += dropped
	}
	batch.Entries = entries
	return batch
}

// carryLocked rescues the entries that must not be lost from a batch that was
// dropped: the generation boundaries, with the resolved commands attached to
// them. Text-bearing entries stay dropped -- they are what dropped_bytes is
// defined to account for.
//
// Past runtimeLogSubMarkerCarry it stops accumulating and resyncs instead.
// dropped_bytes means "bytes the process printed are missing here" and cannot
// honestly carry "a generation boundary was lost"; rather than invent a second
// signal for a reader this far behind, the stream is ended so its reconnect can
// deliver a complete history.
func (s *runtimeLogSub) carryLocked(entries []RuntimeLogEntryDTO) {
	for _, e := range entries {
		if e.Event == "" {
			continue
		}
		if len(s.carry) >= runtimeLogSubMarkerCarry {
			s.resyncLocked()
			return
		}
		s.carry = append(s.carry, e)
	}
}

// resyncLocked ends this subscriber's stream so its reconnect can start over.
func (s *runtimeLogSub) resyncLocked() {
	if s.closed {
		return
	}
	s.closed = true
	s.carry = nil
	close(s.resync)
}

// take stamps the accumulated drop count onto batch, immediately before the SSE
// writer writes it, so a loss recorded while the queue was full is reported on
// the next thing the reader actually sees.
//
// The rescued MARKERS are deliberately not flushed here. A count is a scalar and
// can ride any later batch honestly; a marker has a position, and its position
// is between the batch that was dropped and the next one that fit -- which is
// where deliver puts it. Flushing markers here would put them in front of every
// batch still queued from BEFORE the drop, i.e. in front of older output.
func (s *runtimeLogSub) take(batch RuntimeLogBatchDTO) RuntimeLogBatchDTO {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := s.dropped.Swap(0)
	if dropped == 0 {
		return batch
	}
	// Copy before stamping: the batch value is shared by every subscriber of
	// this spec (publish fans out one snapshot) and each of them has its own
	// losses. The per-entry Command pointers inside are shared and need no deep
	// copy -- once sanitizeRuntimeLogCommand has run on ingest, nothing writes
	// them again.
	entries := make([]RuntimeLogEntryDTO, len(batch.Entries))
	copy(entries, batch.Entries)
	if len(entries) == 0 {
		entries = []RuntimeLogEntryDTO{{}}
	}
	entries[0].DroppedBytes += dropped
	batch.Entries = entries
	return batch
}

// runtimeLogRegistry fans out managed-process output to open portal log views
// and derives, from the set of those views, what it asks each agent to stream.
//
// Deliberately volatile and subscription-scoped: an entry exists only while at
// least one SSE request is in flight for that (server, spec), and the last
// unsubscribe removes it. There is consequently no Retain here, unlike its
// siblings in runtime_registry.go -- there is nothing that can outlive a
// deleted server, because there is nothing that outlives a request.
//
// nil-safe on every method, matching every other per-server registry in this
// package: a bare *Server built directly in a test, bypassing New, must keep
// working.
type runtimeLogRegistry struct {
	mu   sync.Mutex
	subs map[string]map[string]map[*runtimeLogSub]struct{}
	// epochs counts VIEWER ARRIVALS per (server, spec). It is the fact the
	// agent cannot derive for itself: the watch set it receives is identical
	// whether a second operator just opened the same spec or nothing happened at
	// all. Bumped on every subscribe, and on every spec of a server whose agent
	// (re)connects; never on unsubscribe, which owes nobody a replay. Kept
	// exactly as long as the subscriptions are, so a server with no viewers
	// leaves nothing behind (and the agent, told to watch nothing, forgets its
	// own copy at the same moment).
	epochs map[string]map[string]uint64

	// notify tells one server's agent the new full watch set and the epoch of
	// each spec in it. Set once at construction time (gateway.New wires it to
	// AgentStreamRegistry.NotifyRuntimeLogWatch); nil simply means nothing is
	// ever asked to stream, which is the correct degraded behaviour for a
	// Server assembled without an agent-stream registry.
	notify func(serverID string, specIDs []string, epochs map[string]uint64)
}

func newRuntimeLogRegistry() *runtimeLogRegistry {
	return &runtimeLogRegistry{
		subs:   make(map[string]map[string]map[*runtimeLogSub]struct{}),
		epochs: make(map[string]map[string]uint64),
	}
}

// NewRuntimeLogRegistry builds an empty log registry, exported (unlike the
// type) so cmd/gateway can construct the one instance it hands to
// ServerDeps.RuntimeLogs -- the same exported-constructor-over-unexported-type
// pattern NewRuntimeStatusRegistry uses.
func NewRuntimeLogRegistry() *runtimeLogRegistry { return newRuntimeLogRegistry() }

// setNotify installs the "tell the agent what to stream" hook. Called once by
// gateway.New, which is the only place that has both this registry and the
// agent-stream registry in hand.
func (r *runtimeLogRegistry) setNotify(fn func(serverID string, specIDs []string, epochs map[string]uint64)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notify = fn
}

// subscribe registers one portal log view for (serverID, specID) and returns
// it with an idempotent unsubscribe. It reports false, having registered
// nothing, when serverID already watches runtimeLogMaxWatchedSpecs distinct
// specs and this would be one more.
//
// Fan-out is the point: the SECOND operator watching the same spec joins the
// existing agent stream rather than starting another one, because what the
// agent is told is a SET of spec ids, and adding an id already in it produces
// an identical command.
//
// But "the command is identical" is exactly what made the second operator's
// window blank. A history replay is owed to a VIEWER, and a viewer arriving is
// not a set transition -- two tabs on the same crashed spec, a dialog closed and
// reopened faster than the old handler's ctx.Done is processed, an SSE
// connection re-established under a proxy read-timeout: all of them leave the
// set byte-identical, and the agent, keyed to the set, replayed nothing. So this
// bumps that spec's EPOCH on every arrival, and the epoch travels in the command
// (runtimeLogWatchFrame). The already-watching viewer is protected on the way
// back instead of by withholding the request: the replay is routed only to
// subscribers that have not had one (runtimeLogSub.deliver), so the viewer
// mid-stream sees nothing of it. Dropping the id and re-adding it -- the obvious
// alternative -- would race that viewer's live output and tear it.
//
// The agent is notified OUTSIDE the lock: the notify path marshals a frame and
// enqueues it on every open agent connection, and there is no reason to hold a
// lock that every other subscribe/unsubscribe/publish also needs while it does.
func (r *runtimeLogRegistry) subscribe(serverID, specID string) (*runtimeLogSub, func(), bool) {
	sub := &runtimeLogSub{
		ch:     make(chan RuntimeLogBatchDTO, runtimeLogSubBuffer),
		resync: make(chan struct{}),
	}
	if r == nil || serverID == "" || specID == "" {
		return sub, func() {}, true
	}
	r.mu.Lock()
	if byspec := r.subs[serverID]; byspec[specID] == nil && len(byspec) >= runtimeLogMaxWatchedSpecs {
		r.mu.Unlock()
		return sub, func() {}, false
	}
	byspec := r.subs[serverID]
	if byspec == nil {
		byspec = make(map[string]map[*runtimeLogSub]struct{})
		r.subs[serverID] = byspec
	}
	set := byspec[specID]
	if set == nil {
		set = make(map[*runtimeLogSub]struct{})
		byspec[specID] = set
	}
	set[sub] = struct{}{}
	r.bumpEpochLocked(serverID, specID)
	notify, watched, epochs := r.notifyStateLocked(serverID)
	r.mu.Unlock()
	if notify != nil {
		notify(serverID, watched, epochs)
	}

	var once sync.Once
	return sub, func() {
		once.Do(func() {
			r.mu.Lock()
			if byspec, ok := r.subs[serverID]; ok {
				if set, ok := byspec[specID]; ok {
					delete(set, sub)
					if len(set) == 0 {
						delete(byspec, specID)
						// The epoch outlives no subscription: the agent, told to
						// stop watching this spec, forgets its own copy at the
						// same moment, so the two can never disagree.
						if byserver := r.epochs[serverID]; byserver != nil {
							delete(byserver, specID)
							if len(byserver) == 0 {
								delete(r.epochs, serverID)
							}
						}
					}
				}
				if len(byspec) == 0 {
					delete(r.subs, serverID)
				}
			}
			// No bump: an unsubscribe owes nobody a history.
			notify, watched, epochs := r.notifyStateLocked(serverID)
			r.mu.Unlock()
			if notify != nil {
				notify(serverID, watched, epochs)
			}
		})
	}, true
}

// bumpEpochLocked records one viewer arrival for (serverID, specID).
func (r *runtimeLogRegistry) bumpEpochLocked(serverID, specID string) {
	byserver := r.epochs[serverID]
	if byserver == nil {
		byserver = make(map[string]uint64)
		r.epochs[serverID] = byserver
	}
	byserver[specID]++
}

// notifyStateLocked returns the notify hook, serverID's current watch set, and
// the epoch of each spec in it. Sorted so the command the agent receives is
// stable for an unchanged set -- a map iteration order would make every
// subscribe/unsubscribe on an unrelated spec look like a different command.
func (r *runtimeLogRegistry) notifyStateLocked(serverID string) (func(string, []string, map[string]uint64), []string, map[string]uint64) {
	byspec := r.subs[serverID]
	watched := make([]string, 0, len(byspec))
	epochs := make(map[string]uint64, len(byspec))
	for specID := range byspec {
		watched = append(watched, specID)
		epochs[specID] = r.epochs[serverID][specID]
	}
	sort.Strings(watched)
	return r.notify, watched, epochs
}

// restate is what a FRESH agent connection is answered with: serverID's watch
// set, with every spec's epoch bumped.
//
// The bump is the fix for a hole the old restate left open. A plain WebSocket
// reconnect -- the NetBird/WireGuard tunnel drop pingLoop exists for -- leaves
// the agent's own watch map intact, so a restate that only names the same specs
// made the agent skip the snapshot, and any output it drained and could not send
// while the connection was down stayed unreported: the operator saw the output
// before the drop and the output after it, contiguous, with no marker. Bumping
// makes the reconnect genuinely re-snapshot, which repairs the gap from the
// agent's retention buffer (Drain deletes only the send queue, never the
// history) and marks whatever the buffer had since evicted.
func (r *runtimeLogRegistry) restate(serverID string) ([]string, map[string]uint64) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for specID := range r.subs[serverID] {
		r.bumpEpochLocked(serverID, specID)
	}
	_, watched, epochs := r.notifyStateLocked(serverID)
	return watched, epochs
}

// watched reports which specs of serverID currently have at least one open log
// view -- diagnostics and tests; the connect-time restate uses restate.
func (r *runtimeLogRegistry) watched(serverID string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, watched, _ := r.notifyStateLocked(serverID)
	return watched
}

// publish fans one agent-reported batch out to that spec's open log views.
//
// Non-blocking per subscriber, mirroring runtimeStatusRegistry.publish's
// outside-the-lock delivery: the agent-ingest goroutine that called this is
// never blocked by a slow browser. Which subscribers a batch is for, and what
// each of them is told, is runtimeLogSub.deliver's decision -- a history replay
// belongs only to the viewer that asked for one, and a batch that does not fit a
// subscriber's queue loses its text but never its boundary markers. Batches are
// NOT retained for a subscriber that has not arrived yet; the agent's scrollback
// is the only replay, and it is authoritative.
func (r *runtimeLogRegistry) publish(serverID string, batch RuntimeLogBatchDTO) {
	if r == nil || serverID == "" || batch.SpecID == "" {
		return
	}
	r.mu.Lock()
	set := r.subs[serverID][batch.SpecID]
	targets := make([]*runtimeLogSub, 0, len(set))
	for sub := range set {
		targets = append(targets, sub)
	}
	r.mu.Unlock()

	for _, sub := range targets {
		sub.deliver(batch)
	}
}

// runtimeLogEvents is the ALLOW-LIST of boundary markers an agent may report.
// The portal renders each of these as its own localized sentence ("process
// started, pid N" / "process exited, code N" / "process did not start"), which
// is only safe as long as
// the set is closed: an agent -- buggy, outdated, or compromised -- must not
// be able to put a value here that becomes gateway-authored-looking text in
// front of an operator. Anything else degrades to an ordinary entry with no
// marker, which is safe and honest. Same technique, and the same reasoning, as
// runtimeReportParseErrorCodes in agent_runtime.go.
var runtimeLogEvents = map[string]bool{"started": true, "exited": true, "start_failed": true}

// runtimeLogOpeningEvents are the markers that OPEN a generation, and therefore
// the only entries a resolved launch command may be attached to. A command on an
// output line, or on an "exited" marker, describes nothing -- so it is stripped
// rather than relayed, on the same closed-set reasoning as the event kind
// itself.
var runtimeLogOpeningEvents = map[string]bool{"started": true, "start_failed": true}

// runtimeLogMaxAtLen clamps a reported timestamp string. It is passed through
// verbatim (the gateway has no reason to re-serialize the agent's clock), so
// it gets the same length discipline as every other agent-chosen string.
const runtimeLogMaxAtLen = 64

const (
	// runtimeLogMaxCommandFieldLen clamps the two single-string command fields
	// (binary, work_dir). Both are absolute filesystem paths.
	runtimeLogMaxCommandFieldLen = 4 << 10
	// runtimeLogMaxCommandEntries clamps the argv length and the env entry
	// count, each. Far above any real model server's command line, so it
	// guards against a malformed or hostile agent rather than limiting an
	// operator.
	runtimeLogMaxCommandEntries = 512
	// runtimeLogMaxCommandBytes clamps args PLUS env together, which is the
	// number that actually bounds the fan-out: a per-entry cap alone would let
	// 512 entries of 4 KiB through. Deliberately larger than the agent's own
	// 16 KiB budget (server-agent/internal/runtime/command.go), so a
	// well-behaved agent is never re-truncated here and this clamp only ever
	// fires on an agent that ignored its own bound.
	runtimeLogMaxCommandBytes = 32 << 10
)

// sanitizeRuntimeLogCommand bounds every agent-chosen length and count in a
// reported command, and reports whether it dropped anything so Truncated stays
// honest. It does NOT re-mask -- see RuntimeLogCommandDTO for why that is the
// agent's job and cannot be the gateway's.
//
// Entries are dropped whole, never shortened: half an argument is a value that
// looks real and is not, which is the one outcome worse than a missing one.
func sanitizeRuntimeLogCommand(cmd *RuntimeLogCommandDTO) {
	if cmd == nil {
		return
	}
	cmd.Binary = clampString(cmd.Binary, runtimeLogMaxCommandFieldLen)
	cmd.WorkDir = clampString(cmd.WorkDir, runtimeLogMaxCommandFieldLen)
	budget := runtimeLogMaxCommandBytes
	keep := func(in []string) []string {
		if len(in) > runtimeLogMaxCommandEntries {
			in = in[:runtimeLogMaxCommandEntries]
			cmd.Truncated = true
		}
		out := in[:0:0]
		for _, s := range in {
			if len(s) > runtimeLogMaxCommandFieldLen || len(s) > budget {
				cmd.Truncated = true
				continue
			}
			budget -= len(s)
			out = append(out, s)
		}
		return out
	}
	cmd.Args = keep(cmd.Args)
	cmd.Env = keep(cmd.Env)
}

// ingestRuntimeLog parses one agent runtime_log frame and hands it to whoever
// is watching that spec. It is the ONLY path managed-process output takes
// through the gateway, and it deliberately has no store call, no error
// return, and no log statement carrying any part of the payload:
//
//   - no store call, because nothing here is persisted, at all, ever. That is
//     the property the whole feature rests on, and the reason it needs no
//     retention policy;
//   - no error return, because a malformed frame is not worth closing a
//     healthy telemetry connection over (the same latest-wins tolerance the
//     telemetry and system-report frames get);
//   - no logging of content, because this can be prompt text. The router's
//     deliberate refusal to put a body fragment into a decode error is the
//     precedent.
//
// Delivery is best-effort by construction: with nobody watching, publish is a
// no-op, which is also the state the agent should be in (it streams only what
// the gateway asked for), so an unsolicited frame simply costs one parse.
func (s *Server) ingestRuntimeLog(serverID string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var batch RuntimeLogBatchDTO
	if err := json.Unmarshal(raw, &batch); err != nil {
		// The error text is not logged: json decode errors quote the offending
		// input, which here is process output.
		slog.Debug("agent stream: invalid runtime_log frame", "server_id", serverID)
		return
	}
	batch.SpecID = clampString(batch.SpecID, runtimeLogMaxSpecIDLen)
	if batch.SpecID == "" {
		return
	}
	if len(batch.Entries) > runtimeLogMaxEntries {
		batch.Entries = batch.Entries[:runtimeLogMaxEntries]
	}
	for i := range batch.Entries {
		batch.Entries[i].At = clampString(batch.Entries[i].At, runtimeLogMaxAtLen)
		if !runtimeLogEvents[batch.Entries[i].Event] {
			batch.Entries[i].Event = ""
			batch.Entries[i].ExitCode = 0
		}
		if batch.Entries[i].DroppedBytes < 0 {
			batch.Entries[i].DroppedBytes = 0
		}
		if !runtimeLogOpeningEvents[batch.Entries[i].Event] {
			batch.Entries[i].Command = nil
		}
		sanitizeRuntimeLogCommand(batch.Entries[i].Command)
	}
	if batch.Entries == nil {
		batch.Entries = []RuntimeLogEntryDTO{}
	}
	s.RuntimeLogs.publish(serverID, batch)
}

// clampString truncates s to at most n bytes. Byte-oriented (not rune-
// oriented) on purpose: it guards a map key and a wire field length, and the
// values it guards -- ids and timestamps -- are ASCII by construction.
func clampString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// batchTextBytes is how much process output a dropped batch was carrying --
// the number that becomes the reader's "N bytes missing" marker. Only text
// counts: a lost boundary marker is not a byte the process printed, and
// inflating the count with bookkeeping would misreport the gap.
func batchTextBytes(batch RuntimeLogBatchDTO) int64 {
	var n int64
	for _, e := range batch.Entries {
		n += int64(len(e.Text))
	}
	return n
}
