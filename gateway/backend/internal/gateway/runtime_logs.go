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
	runtimeLogMaxSpecIDLen = 128
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
// Masked says at least one value was replaced by its ${AGENT_ENV:NAME}
// placeholder. Truncated says entries are missing -- set by the agent when the
// command exceeded its own cap, and by sanitizeRuntimeLogCommand when it
// exceeded the gateway's. Args and Env are agent/operator-authored strings:
// render them as text, never as HTML.
type RuntimeLogCommandDTO struct {
	Binary    string   `json:"binary,omitempty"`
	Args      []string `json:"args,omitempty"`
	WorkDir   string   `json:"work_dir,omitempty"`
	Env       []string `json:"env,omitempty"`
	Masked    bool     `json:"masked,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
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
	Entries    []RuntimeLogEntryDTO `json:"entries"`
}

// runtimeLogSub is one open portal log view: a bounded queue plus the count of
// bytes dropped because that queue was full. The counter is separate from the
// queue on purpose -- a full queue must not need a slot to record that it was
// full.
type runtimeLogSub struct {
	ch      chan RuntimeLogBatchDTO
	dropped atomic.Int64
}

// take swaps out the accumulated drop count and stamps it onto batch, so the
// loss is reported at the exact position it happened. Called by the SSE writer
// immediately before each write.
func (s *runtimeLogSub) take(batch RuntimeLogBatchDTO) RuntimeLogBatchDTO {
	dropped := s.dropped.Swap(0)
	if dropped == 0 {
		return batch
	}
	if len(batch.Entries) == 0 {
		batch.Entries = []RuntimeLogEntryDTO{{DroppedBytes: dropped}}
		return batch
	}
	// Copy before stamping: the batch value is shared by every subscriber of
	// this spec (publish fans out one snapshot), and each of them has its own
	// drop count. Mutating in place would give one subscriber's gap marker to
	// all of them. The per-entry Command pointers inside are shared and need no
	// deep copy: once sanitizeRuntimeLogCommand has run on ingest, nothing
	// writes them again.
	entries := make([]RuntimeLogEntryDTO, len(batch.Entries))
	copy(entries, batch.Entries)
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

	// notify tells one server's agent the new full watch set. Set once at
	// construction time (gateway.New wires it to
	// AgentStreamRegistry.NotifyRuntimeLogWatch); nil simply means nothing is
	// ever asked to stream, which is the correct degraded behaviour for a
	// Server assembled without an agent-stream registry.
	notify func(serverID string, specIDs []string)
}

func newRuntimeLogRegistry() *runtimeLogRegistry {
	return &runtimeLogRegistry{subs: make(map[string]map[string]map[*runtimeLogSub]struct{})}
}

// NewRuntimeLogRegistry builds an empty log registry, exported (unlike the
// type) so cmd/gateway can construct the one instance it hands to
// ServerDeps.RuntimeLogs -- the same exported-constructor-over-unexported-type
// pattern NewRuntimeStatusRegistry uses.
func NewRuntimeLogRegistry() *runtimeLogRegistry { return newRuntimeLogRegistry() }

// setNotify installs the "tell the agent what to stream" hook. Called once by
// gateway.New, which is the only place that has both this registry and the
// agent-stream registry in hand.
func (r *runtimeLogRegistry) setNotify(fn func(serverID string, specIDs []string)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notify = fn
}

// subscribe registers one portal log view for (serverID, specID) and returns
// it with an idempotent unsubscribe.
//
// Fan-out is the point: the SECOND operator watching the same spec joins the
// existing agent stream rather than starting another one, because what the
// agent is told is a SET of spec ids, and adding an id already in it produces
// an identical command. Only the transitions that actually change the set --
// the first subscriber arriving, the last one leaving -- change what the agent
// does.
//
// The agent is notified OUTSIDE the lock: the notify path marshals a frame and
// enqueues it on every open agent connection, and there is no reason to hold a
// lock that every other subscribe/unsubscribe/publish also needs while it does.
func (r *runtimeLogRegistry) subscribe(serverID, specID string) (*runtimeLogSub, func()) {
	sub := &runtimeLogSub{ch: make(chan RuntimeLogBatchDTO, runtimeLogSubBuffer)}
	if r == nil || serverID == "" || specID == "" {
		return sub, func() {}
	}
	r.mu.Lock()
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
	notify, watched := r.notifyStateLocked(serverID)
	r.mu.Unlock()
	if notify != nil {
		notify(serverID, watched)
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
					}
				}
				if len(byspec) == 0 {
					delete(r.subs, serverID)
				}
			}
			notify, watched := r.notifyStateLocked(serverID)
			r.mu.Unlock()
			if notify != nil {
				notify(serverID, watched)
			}
		})
	}
}

// notifyStateLocked returns the notify hook and serverID's current watch set.
// Sorted so the command the agent receives is stable for an unchanged set --
// a map iteration order would make every subscribe/unsubscribe on an unrelated
// spec look like a different command.
func (r *runtimeLogRegistry) notifyStateLocked(serverID string) (func(string, []string), []string) {
	byspec := r.subs[serverID]
	watched := make([]string, 0, len(byspec))
	for specID := range byspec {
		watched = append(watched, specID)
	}
	sort.Strings(watched)
	return r.notify, watched
}

// watched reports which specs of serverID currently have at least one open log
// view. Used on a fresh agent connection to restate the desired set -- see
// handleAgentStream.
func (r *runtimeLogRegistry) watched(serverID string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, watched := r.notifyStateLocked(serverID)
	return watched
}

// publish fans one agent-reported batch out to that spec's open log views.
//
// Non-blocking per subscriber, mirroring runtimeStatusRegistry.publish's
// outside-the-lock delivery: a subscriber whose queue is full has its batch
// dropped and the bytes counted (surfaced on its next delivered batch), and
// the agent-ingest goroutine that called this is never blocked by a slow
// browser. Batches are NOT retained for a subscriber that has not arrived yet;
// the agent's scrollback is the only replay, and it is authoritative.
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
		select {
		case sub.ch <- batch:
		default:
			sub.dropped.Add(batchTextBytes(batch))
		}
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
