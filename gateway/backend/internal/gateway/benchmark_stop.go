// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"time"
)

// The reasons a stop-all logs when it stops nothing for a target, or when what
// it stopped was not confirmed quiet. Each is the "reason" attribute of an Info
// log; none is an error, and the target's result then carries no load time.
// formerly_pinned is a spec the run unpinned, other than the target, that reads
// something other than stopped with pid 0 when the run has no sign that the
// agent holds the lifted pins (benchmarkStopSet). Its Info is a message of its
// own, logBenchmarkLoadTimeNotMeasured's, which names those specs.
const (
	benchmarkStopReasonInFlight             = "in_flight"
	benchmarkStopReasonNoFrame              = "no_frame"
	benchmarkStopReasonCanceled             = "canceled"
	benchmarkStopReasonNoSpec               = "no_spec"
	benchmarkStopReasonNoStatus             = "no_status"
	benchmarkStopReasonSpecUnreadable       = "spec_unreadable"
	benchmarkStopReasonUnknownSpec          = "unknown_spec"
	benchmarkStopReasonDisabled             = "disabled"
	benchmarkStopReasonPinned               = "pinned"
	benchmarkStopReasonAdminOverride        = "admin_override"
	benchmarkStopReasonIsolationUnavailable = "isolation_unavailable"
	benchmarkStopReasonPartialStop          = "partial_stop"
	benchmarkStopReasonStopTimeout          = "stop_timeout"
	benchmarkStopReasonLateStart            = "late_start"
	benchmarkStopReasonFormerlyPinned       = "formerly_pinned"
)

// benchmarkStopWaitBound: the 60 s poll that delivers the override to an agent
// without a WebSocket (vramIsolationBindDelay), plus the drain
// (vramIsolationDrainBound). Initialised from the two, so they cannot drift; a
// test shrinks this var itself, set before the run starts and restored only
// after the run has finished (the VRAM tests' pattern; a run goroutine that
// outlives the restore would race).
var benchmarkStopWaitBound = vramIsolationBindDelay + vramIsolationDrainBound

// benchmarkMayPreStop marks every target of a manual speed or both run
// (startBenchmark) as one whose cold pass may first stop the server's running
// agent models (mayPreStop, preStopServer). Any other mode marks nothing, and no
// other starter calls it: a scheduled, capacity, vision, Load, VRAM or
// context-probe run never runs the stop-all (a VRAM run force-stops the launch
// specs through its own drain, vramDrain).
func benchmarkMayPreStop(targets []benchmarkTarget, mode string) {
	if mode != "speed" && mode != "both" {
		return
	}
	for i := range targets {
		targets[i].mayPreStop = true
	}
}

// benchmarkRowQuiet reports whether a status row's spec has no process: a
// state vramStateNoProcess recognizes, AND pid 0. The state alone is not
// enough, because start_failed can still carry a live child for up to the
// agent's kill grace. An unrecognized state is not quiet, which is the
// fail-closed direction. It is the stop path's rule only: there the run has
// unpinned every enabled pinned spec (beginBenchmarkUnpin; a disabled spec is
// not in the agent's document) and refuses to stop next to force_running, so,
// once the agent holds the lifted pins, a neighbour without a process stays
// down without a request. The stop path confirms a cold start only when a
// stopped spec that was not quiet has turned quiet, which the run takes as the
// sign that the agent does, or when every formerly pinned neighbour reads
// stopped with pid 0 (benchmarkStopSet, which names the sign's one limit). A
// run without a stop uses the stricter benchmarkOthersStopped.
func benchmarkRowQuiet(row RuntimeStatusDTO) bool {
	return vramStateNoProcess(row.State) && row.PID == 0
}

// preStopServer is the stop-all before a manual speed or both run's agent
// target's cold pass. It stops every running model of the server's agent
// application, the target included, waits until the agent reports them all
// without a process, and clears the overrides again before it returns, so the
// cold pass measures the model's own load on an otherwise empty server.
//
// The sequence, each step on the target's own decision:
//   - the selection frame and the stop set (benchmarkStopSet): a fresh status
//     frame without traffic in flight, and the spec ids that are not quiet,
//     plus the target whenever its own row reads anything other than stopped.
//     Without a row in the stop set that is not quiet (benchmarkRowQuiet: a
//     process, or a state this gateway does not recognize), the run has no
//     sign that the agent holds the lifted pins, so every formerly pinned
//     neighbour, the target aside, has to read stopped with pid 0 for the
//     start to be confirmed;
//   - eligibility (benchmarkStopRefusal), every check before anything is
//     written. A failed check stops nothing, and stops stay on for the next
//     target;
//   - the lease: the stop set is recorded in the override lease before the
//     write, so no override is ever written without a lease entry that names
//     it;
//   - one force_stopped batch with one notification, and a deferred clear of
//     every spec it wrote or may have written (clearBenchmarkStop);
//   - the wait (benchmarkAwaitQuiet), only when every spec was written. When
//     the target's own row read backoff, start_failed or crashed, it also
//     waits for that row to read stopped with pid 0;
//   - after a partial stop or a late start, when the target's own write
//     succeeded, a wait until the target's own row reads quiet
//     (benchmarkAwaitSpecQuiet), so the stop lands before the cold pass. An
//     expired wait, like an expired benchmarkAwaitQuiet, turns stops off.
//
// Neither an ineligible stop, nor an unconfirmed or taken-over one, is an
// error: the result is coldStart{} or an unconfirmed start, and the mapping
// keeps its last load time. The one error is a clear that failed, which
// clearBenchmarkStop turns into this result's error. rideOutStop is set
// whenever the target's own override was or may have been written, so its cold
// pass rides the router's 503 until the clear reaches the agent.
func (s *Server) preStopServer(ctx context.Context, tgt benchmarkTarget) (cs coldStart, err error) {
	serverID, ov := tgt.server.ID, tgt.overrides
	sel, reason := s.benchmarkStopSet(ctx, tgt)
	stopSet := sel.stopSet
	if reason == "" && len(stopSet) == 0 {
		return coldStart{confirmed: true}, nil
	}
	if reason == "" {
		reason = s.benchmarkStopRefusal(ctx, tgt, stopSet)
	}
	if reason != "" {
		logBenchmarkLoadTimeNotMeasured("benchmark: running agent models not stopped; load time not measured", tgt.mapping.ID, reason, sel.formerPins)
		return coldStart{}, nil
	}
	owedBefore := ov.stopOwed
	ov.stopOwed = benchmarkUnion(owedBefore, stopSet)
	if lerr := s.writeBenchmarkLease(ctx, serverID, ov); lerr != nil {
		ov.stopOwed = owedBefore
		slog.Warn("benchmark: could not record the override lease; running agent models are not stopped for this target", "mapping_id", tgt.mapping.ID, "err", lerr)
		return coldStart{}, nil
	}
	out, _ := s.Portal.SetBenchmarkRuntimeSpecsAdminState(ctx, stopSet, "", vramAdminStateForceStopped)
	// A failed write may have stored the override before its GPU write failed,
	// so a failed spec is cleared as well; the compare-and-set makes that
	// harmless for a spec that was never written. Gone and Conflict leave the
	// owed set, because nothing was written for them.
	owed := benchmarkUnion(out.Written, out.Failed)
	ov.stopOwed = benchmarkWithout(ov.stopOwed, benchmarkUnion(out.Gone, out.Conflict))
	if len(owed) > 0 {
		defer s.clearBenchmarkStop(ctx, serverID, ov, owed, &cs, &err)
		ov.stopped = benchmarkUnion(ov.stopped, owed)
		ov.run.setStopped(ov.stopped)
		s.Benchmarks.publish(serverID, ov.run.snapshot())
	}
	targetStopped := slices.Contains(owed, tgt.spec.ID)
	if len(out.Written) != len(stopSet) {
		reason = benchmarkStopReasonPartialStop
	} else if quiet, waitReason := s.benchmarkAwaitQuiet(ctx, serverID, stopSet, sel.awaitStopped); !quiet {
		reason = waitReason
	} else if !sel.pinsSettled {
		reason = benchmarkStopReasonFormerlyPinned
	}
	if reason == "" {
		return coldStart{confirmed: true, rideOutStop: targetStopped}, nil
	}
	if reason == benchmarkStopReasonStopTimeout {
		// A child that never exits costs the bound once per run, not once per
		// target.
		ov.stopsAllowed = false
	}
	// Without the wait, or after a late start ended it, the stop may not have
	// reached the agent yet. A cold pass the still-running target served now
	// would leave the stop to land between the cold and the warm pass, and the
	// warm pass would fail on the router's 503. So the cold pass starts only
	// once the target's own row reads quiet.
	if (reason == benchmarkStopReasonPartialStop || reason == benchmarkStopReasonLateStart) && slices.Contains(out.Written, tgt.spec.ID) && !s.benchmarkAwaitSpecQuiet(ctx, serverID, tgt.spec.ID) {
		ov.stopsAllowed = false
	}
	logBenchmarkLoadTimeNotMeasured("benchmark: the stopped agent models were not confirmed quiet; load time not measured", tgt.mapping.ID, reason, sel.formerPins)
	return coldStart{rideOutStop: targetStopped}, nil
}

// logBenchmarkLoadTimeNotMeasured logs, at Info, why a stop-all leaves the
// target without a load time: msg with the reason, or, for formerly_pinned,
// a message of its own that names the formerly pinned neighbours that read
// other than stopped with pid 0 (formerPins).
func logBenchmarkLoadTimeNotMeasured(msg, mappingID, reason string, formerPins []string) {
	if reason == benchmarkStopReasonFormerlyPinned {
		slog.Info("benchmark: a launch spec the run unpinned may still start by itself; load time not measured", "mapping_id", mappingID, "reason", reason, "spec_ids", formerPins)
		return
	}
	slog.Info(msg, "mapping_id", mappingID, "reason", reason)
}

// clearBenchmarkStop is preStopServer's deferred clear of owed, the specs its
// stop batch wrote or may have written. It runs when preStopServer returns, so
// always before the cold pass, and on a context that is not cancelled with the
// run (restoreBenchmarkOverrides). It then rewrites the lease, on a context of
// its own as well. When that rewrite succeeds, the lease's clear_force_stopped
// names only the specs whose clear failed, and its repin still names what the
// run unpinned; when it fails, the row keeps what it named before, and
// endBenchmarkUnpin rewrites it again at the run's end.
//
// A failed clear turns *cs into coldStart{} and *err into the result's error,
// and turns stops off for the rest of the run: a store that fails a write
// should not collect more overrides. The error says the specs may still be
// force_stopped, because a clear can also fail after it stored the cleared row
// (its GPU rows or its read-back failed). A taken-over clear (the spec carries
// another admin_state: somebody else owns the field now, or the stop's failed
// write never stored it) leaves the start unconfirmed.
func (s *Server) clearBenchmarkStop(ctx context.Context, serverID string, ov *benchmarkOverrides, owed []string, cs *coldStart, err *error) {
	failed, takenOver := s.restoreBenchmarkOverrides(ctx, owed)
	ov.stopOwed = benchmarkWithout(ov.stopOwed, benchmarkWithout(owed, failed))
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vramRestoreTimeout)
	if lerr := s.writeBenchmarkLease(lctx, serverID, ov); lerr != nil {
		slog.Warn("benchmark: could not update the override lease after a clear", "server_id", serverID, "err", lerr)
	}
	cancel()
	switch {
	case len(failed) > 0:
		ov.stopsAllowed = false
		*cs, *err = coldStart{}, fmt.Errorf("launch specs may still be force_stopped after the benchmark: %s; clear the overrides in the runtime section", strings.Join(failed, ", "))
	case len(takenOver) > 0:
		cs.confirmed = false
	}
}

// benchmarkStopSet is the stop set of tgt's stop-all, read from a fresh
// selection frame (benchmarkSelectionFrame): the sorted spec ids of the rows
// that are not quiet (benchmarkRowQuiet), which are running, starting or
// draining, carry a pid, or read a state this gateway does not recognize.
//
// The target is in it when it is resident, and also whenever its own row reads
// anything other than stopped (backoff, start_failed, crashed, not_permitted,
// pending_vram_unknown). A request for a spec in backoff waits for its backoff
// timer, which would land in the cold pass. The agent resets backoff,
// start_failed and crashed to stopped when it applies the stop's document,
// which can be as late as its poll, and the quiet wait waits for that
// (benchmarkStopSelection.awaitStopped, benchmarkStatesAwaitStopped). Applying
// a document never resets not_permitted or pending_vram_unknown, and a request
// for those waits behind no timer. Every other quiet row is left alone, since
// writing it would only widen the override's footprint.
//
// reason is set when nothing can be stopped for this target: no_spec (the
// target has no launch spec), no_status (the frame has no row for it), or the
// selection frame's own reason. With an empty stop set and pinsSettled false,
// it is formerly_pinned: the start cannot be confirmed.
func (s *Server) benchmarkStopSet(ctx context.Context, tgt benchmarkTarget) (sel benchmarkStopSelection, reason string) {
	if tgt.spec.ID == "" {
		return sel, benchmarkStopReasonNoSpec
	}
	rows, reason := s.benchmarkSelectionFrame(ctx, tgt.server.ID)
	if reason != "" {
		return sel, reason
	}
	own, ok := benchmarkSpecStatus(rows, tgt.spec.ID)
	if !ok {
		return sel, benchmarkStopReasonNoStatus
	}
	var stopSet []string
	for _, row := range rows {
		if !benchmarkRowQuiet(row) || (row.SpecID == tgt.spec.ID && row.State != agentSpecStateStopped) {
			stopSet = append(stopSet, row.SpecID)
		}
	}
	if benchmarkStatesAwaitStopped[own.State] {
		sel.awaitStopped = tgt.spec.ID
	}
	drains := slices.ContainsFunc(rows, func(row RuntimeStatusDTO) bool { return !benchmarkRowQuiet(row) })
	sel.formerPins = benchmarkFormerPinsNotStopped(rows, tgt.overrides.unpinned, tgt.spec.ID)
	sel.pinsSettled = drains || len(sel.formerPins) == 0
	if len(stopSet) == 0 && !sel.pinsSettled {
		return sel, benchmarkStopReasonFormerlyPinned
	}
	sel.stopSet = benchmarkUnion(stopSet, nil)
	return sel, ""
}

// benchmarkStopSelection is what a stop-all reads from its selection frame
// (benchmarkStopSet).
type benchmarkStopSelection struct {
	// stopSet is the sorted ids of the specs the stop-all stops.
	stopSet []string
	// pinsSettled reports whether the run may take it that no spec the run
	// unpinned, the target aside, can start a child by itself inside the cold
	// pass. The run does not wait for its unpin to reach the agent, and until
	// it does, such a spec is still pinned there: one without a process in
	// backoff, start_failed or another state than stopped can restart by
	// itself, as when its backoff timer fires. pinsSettled is true when the
	// frame has a row that is not quiet (benchmarkRowQuiet: a process, or a
	// state this gateway does not recognize), which is in the stop set: the
	// run takes that row turning quiet in the wait as the sign that the agent
	// applied the stop's document, which follows the unpin's and carries the
	// lifted pins as well. That sign has one limit: a process that exits by
	// itself inside the document's delivery window, as an idle unload or a
	// crash does, is taken for the stop, and a formerly pinned neighbour still
	// pinned at the agent can then restart; it takes both at once. Otherwise
	// pinsSettled is true only when formerPins is empty.
	pinsSettled bool
	// formerPins is the sorted ids of the specs the run unpinned, the target
	// aside, whose rows read other than stopped with pid 0
	// (benchmarkFormerPinsNotStopped).
	formerPins []string
	// awaitStopped is the target's spec id when its own row read backoff,
	// start_failed or crashed (benchmarkStatesAwaitStopped), and "" otherwise.
	// The quiet wait then also needs that row to read stopped with pid 0
	// (benchmarkStopFrameVerdict): a request for a spec in backoff waits behind
	// its backoff timer, and that wait would land in the load time.
	awaitStopped string
}

// benchmarkStatesAwaitStopped are the process-less states of a target's own
// row in which the stop-all's quiet wait also waits for that row to read
// stopped with pid 0. They are the states the agent resets to stopped when it
// applies a changed spec without a process, as the stop's force_stopped is,
// and backoff also turns stopped when its timer fires without a start; either
// way the request that follows starts the target from cold at once. Until
// then, a request for a spec in backoff queues behind the timer.
// not_permitted and pending_vram_unknown are not in the set: the agent does
// not reset them on an apply, and a request for such a spec waits behind no
// timer. Within the agent's retry interval it gets the cached refusal at once
// (not_permitted a 502, which fails the cold pass; pending_vram_unknown the
// router's 503, which the cold pass after a stop rides, keeping only the
// served attempt's time), and after it the request evaluates the spec again
// at once.
var benchmarkStatesAwaitStopped = map[string]bool{"backoff": true, "start_failed": true, "crashed": true}

// benchmarkFormerPinsNotStopped returns the sorted ids of the specs in
// unpinned, the specs the run unpinned, whose rows read other than stopped
// with pid 0, the row of targetID aside: the rule benchmarkOthersStopped
// applies to every neighbour of a run without a stop. It judges only the specs
// the run unpinned, so a spec somebody else unpinned just before or during the
// run, whose document the agent has not applied yet, is not judged. A formerly
// pinned spec the frame has no row for is not in the agent's document, so the
// agent does not run it.
func benchmarkFormerPinsNotStopped(rows []RuntimeStatusDTO, unpinned []string, targetID string) []string {
	var ids []string
	for _, row := range rows {
		if row.SpecID == targetID || !slices.Contains(unpinned, row.SpecID) {
			continue
		}
		if row.State != agentSpecStateStopped || row.PID != 0 {
			ids = append(ids, row.SpecID)
		}
	}
	slices.Sort(ids)
	return ids
}

// benchmarkSelectionFrame subscribes to serverID's runtime status, discards the
// snapshot the subscription starts from, and returns the first fresh frame in
// which no row has traffic in flight, within benchmarkTelemetryContextWait. A
// fresh frame is needed because the agent emits no frame when a request ends:
// right after the previous target's pass, the snapshot can still show it in
// flight.
//
// A live request the frames show is never cut off: when every frame in the
// window has traffic, the reason is in_flight and nothing is stopped. No frame
// at all is no_frame, and a done ctx or a closed stream is canceled.
func (s *Server) benchmarkSelectionFrame(ctx context.Context, serverID string) (rows []RuntimeStatusDTO, reason string) {
	_, frames, unsub := s.RuntimeStatus.subscribe(serverID)
	defer unsub()
	timer := time.NewTimer(benchmarkTelemetryContextWait)
	defer timer.Stop()
	reason = benchmarkStopReasonNoFrame
	for {
		select {
		case <-ctx.Done():
			return nil, benchmarkStopReasonCanceled
		case <-timer.C:
			return nil, reason
		case frame, open := <-frames:
			if !open {
				return nil, benchmarkStopReasonCanceled
			}
			if !benchmarkFrameHasTraffic(frame) {
				return frame, ""
			}
			reason = benchmarkStopReasonInFlight
		}
	}
}

// benchmarkFrameHasTraffic reports whether any row of frame has a request in
// flight.
func benchmarkFrameHasTraffic(frame []RuntimeStatusDTO) bool {
	return slices.ContainsFunc(frame, func(row RuntimeStatusDTO) bool { return row.InFlight > 0 })
}

// benchmarkStopRefusal runs the stop-all's eligibility checks for the target
// and for every spec of stopSet, all of them before anything is written, from
// one re-read of the application's launch specs. It returns the reason of the
// first failed check, or "" when the stop may go ahead.
//
// The spec is re-read rather than taken from tgt.spec, which was read before the
// run reserved the server and is stale once the run has changed the spec. A
// pinned spec would restart at the clear, a disabled or unknown one is not the
// agent's to stop, and an override is an operator's or another run's. The
// isolation gate (vramIsolationUnavailable) is checked again, because telemetry
// ingest can flip it during the run.
func (s *Server) benchmarkStopRefusal(ctx context.Context, tgt benchmarkTarget, stopSet []string) string {
	specs, err := s.Routes.RuntimeSpecsByApplication(ctx, tgt.app.ID)
	if err != nil {
		return benchmarkStopReasonSpecUnreadable
	}
	byID := make(map[string]routing.RuntimeSpec, len(specs))
	for _, spec := range specs {
		byID[spec.ID] = spec
	}
	for _, id := range benchmarkUnion(stopSet, []string{tgt.spec.ID}) {
		if reason := benchmarkSpecStopRefusal(byID, id); reason != "" {
			return reason
		}
	}
	if _, unavailable := s.vramIsolationUnavailable(ctx, tgt.server.ID); unavailable {
		return benchmarkStopReasonIsolationUnavailable
	}
	return ""
}

// benchmarkSpecStopRefusal is benchmarkStopRefusal's check of one spec id
// against the re-read specs.
func benchmarkSpecStopRefusal(byID map[string]routing.RuntimeSpec, id string) string {
	spec, ok := byID[id]
	switch {
	case !ok:
		return benchmarkStopReasonUnknownSpec
	case !spec.Enabled:
		return benchmarkStopReasonDisabled
	case spec.Pinned:
		return benchmarkStopReasonPinned
	case strings.TrimSpace(spec.AdminState) != "":
		return benchmarkStopReasonAdminOverride
	default:
		return ""
	}
}

// benchmarkAwaitQuiet waits, after the stop batch, until the agent reports
// every spec of stopSet without a process (benchmarkRowQuiet). It subscribes
// after the write and discards the snapshot the subscription starts from.
//
//   - The first frame that has a row for every stopSet spec and in which every
//     row is quiet returns (true, ""). backoff with pid 0 counts: it turns
//     stopped when its timer fires, and the clear resets it at once. Only the
//     target's own row, when awaitStopped names it, has to read stopped with
//     pid 0 as well (benchmarkStopSelection.awaitStopped).
//   - A frame in which a row outside stopSet is not quiet returns (false,
//     late_start) at once: the run has unpinned every enabled pinned spec, and
//     an unpinned spec without force_running starts only on a request, so a
//     direct client started it, or a pinned one restarted by itself before
//     the agent applied the unpin. Waiting would not help.
//   - A done ctx or a closed stream returns (false, canceled), and
//     benchmarkStopWaitBound returns (false, stop_timeout).
//
// A quiet frame is a state, not a transition. It is enough here because an
// unpinned spec without force_running that has no process starts only on a
// request, and routing sends none to a reserved server. The run takes a quiet
// frame in which a stop-set row that was not quiet has turned quiet as the
// sign that the agent applied the stop's document, which follows the unpin's
// and carries the lifted pins as well. A process that exits by itself inside
// the document's delivery window is taken for the stop, and a formerly pinned
// neighbour still pinned at the agent can then restart; it takes both at
// once. A stop set without such a row gives no sign, and preStopServer then
// confirms only when every formerly pinned neighbour read stopped with pid 0
// (benchmarkStopSet).
func (s *Server) benchmarkAwaitQuiet(ctx context.Context, serverID string, stopSet []string, awaitStopped string) (quiet bool, reason string) {
	_, frames, unsub := s.RuntimeStatus.subscribe(serverID)
	defer unsub()
	timer := time.NewTimer(benchmarkStopWaitBound)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, benchmarkStopReasonCanceled
		case <-timer.C:
			return false, benchmarkStopReasonStopTimeout
		case frame, open := <-frames:
			if !open {
				return false, benchmarkStopReasonCanceled
			}
			if done, quiet, reason := benchmarkStopFrameVerdict(frame, stopSet, awaitStopped); done {
				return quiet, reason
			}
		}
	}
}

// benchmarkStopFrameVerdict is benchmarkAwaitQuiet's reading of one frame:
// done when the frame settles the wait, quiet when it settles it as quiet. A
// late start anywhere in the frame settles it, whatever the stop set's rows
// read. When awaitStopped names a spec, the frame settles the wait as quiet
// only once that spec's row reads stopped. The spec is the target, which is in
// the stop set, so its row is then also quiet, pid 0 included.
func benchmarkStopFrameVerdict(frame []RuntimeStatusDTO, stopSet []string, awaitStopped string) (done, quiet bool, reason string) {
	quietInSet := 0
	stoppedAwaited := awaitStopped == ""
	for _, row := range frame {
		inSet := slices.Contains(stopSet, row.SpecID)
		switch {
		case !inSet && !benchmarkRowQuiet(row):
			return true, false, benchmarkStopReasonLateStart
		case inSet && benchmarkRowQuiet(row):
			quietInSet++
		}
		if row.SpecID == awaitStopped && row.State == agentSpecStateStopped {
			stoppedAwaited = true
		}
	}
	settled := quietInSet == len(stopSet) && stoppedAwaited
	return settled, settled, ""
}

// benchmarkAwaitSpecQuiet waits, within benchmarkStopWaitBound, for a fresh
// frame in which specID's own row is quiet (benchmarkRowQuiet), whatever the
// other rows read. It reports false when the bound ran out first, or when ctx
// ended or the stream closed.
func (s *Server) benchmarkAwaitSpecQuiet(ctx context.Context, serverID, specID string) bool {
	_, frames, unsub := s.RuntimeStatus.subscribe(serverID)
	defer unsub()
	timer := time.NewTimer(benchmarkStopWaitBound)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case frame, open := <-frames:
			if !open {
				return false
			}
			if row, ok := benchmarkSpecStatus(frame, specID); ok && benchmarkRowQuiet(row) {
				return true
			}
		}
	}
}

// restoreBenchmarkOverrides clears the force_stopped overrides a benchmark
// wrote on specIDs, as one batch with one notification
// (SetBenchmarkRuntimeSpecsAdminState, force_stopped -> ""). It runs on a
// context of its own, bounded by vramRestoreTimeout and not cancelled with the
// run, so a cancelled run still clears. Its callers are the stop-all's
// deferred clear (clearBenchmarkStop) and runVRAMProbe's restore defer, which
// clears what the VRAM run's drain wrote or may have written.
//
// Written and Gone owe nothing more. A Gone spec is one no benchmark write can
// reach again: a deleted spec has no row, and a retyped one (its application
// is no longer server_agent) leaves the runtime-config document and keeps its
// stored force_stopped, which the portal names in a Warn of its own and which
// acts again only if the application is retyped back. A Conflict is a spec
// whose admin_state is no longer force_stopped: somebody else took the field
// over during the run, the stop's or the drain's failed write never stored
// it, or the VRAM run's clear of its target failed after it stored the
// cleared row (VRAMReport.RestoreTakenOver). It is left alone and returned in
// takenOver with an Info log. Every other failure is returned in failed with a
// Warn log, ErrMappingNotFound included, because the portal returns it for a
// store error too.
func (s *Server) restoreBenchmarkOverrides(ctx context.Context, specIDs []string) (failed, takenOver []string) {
	if len(specIDs) == 0 {
		return nil, nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vramRestoreTimeout)
	defer cancel()
	out, _ := s.Portal.SetBenchmarkRuntimeSpecsAdminState(rctx, specIDs, vramAdminStateForceStopped, "")
	for _, id := range out.Conflict {
		slog.Info("benchmark: a clear found another admin_state on a launch spec and left it alone: taken over during the run, never stopped, or already cleared", "spec_id", id)
	}
	for _, id := range out.Failed {
		slog.Warn("benchmark: could not restore an admin override", "spec_id", id, "err", out.Errs[id])
	}
	return out.Failed, out.Conflict
}

// coldPassAfterStop is the cold pass of a target whose own force_stopped was
// or may have been written (coldStart.rideOutStop). The router answers 503
// runtime.admission_blocked until the clear reaches the agent, and the load
// loop (loadUntilServable) retries only that error. It keeps the TTFT of the
// attempt that was served, which includes the whole start. A swapper that
// answers 503 while it loads would lose part of its load this way, so no other
// cold pass uses it.
func (s *Server) coldPassAfterStop(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request) (time.Duration, error) {
	var ttft time.Duration
	err := s.loadUntilServable(ctx, target, func(ctx context.Context, b loadBound) error {
		d, _, err := s.streamOnceWithin(ctx, streamer, target, req, loadAttemptBudget(b.budget, b.idle, time.Until(b.deadline)))
		if err == nil {
			ttft = d
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("cold pass after the benchmark's stop: %w", err)
	}
	return ttft, nil
}

// benchmarkUnion returns the sorted union of a and b without duplicates, in a
// new slice.
func benchmarkUnion(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// benchmarkWithout returns the sorted ids of a that are not in b, in a new
// slice.
func benchmarkWithout(a, b []string) []string {
	out := make([]string, 0, len(a))
	for _, id := range a {
		if !slices.Contains(b, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
