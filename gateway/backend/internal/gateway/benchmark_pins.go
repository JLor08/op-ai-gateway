// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"time"
)

// benchmarkLeaseReconcileTimeout bounds the reconcile of one server's override
// lease: its two batches and the rewrite of its row.
const benchmarkLeaseReconcileTimeout = 30 * time.Second

// benchmarkLeaseList is one list of the override lease, as the reconciler
// logs it.
type benchmarkLeaseList struct {
	field   string // the list's JSON field in the lease row
	settled string // the Warn that names the specs the reconcile wrote
	failed  string // the Warn that names the specs whose write failed, with the next action
}

var (
	benchmarkLeaseClears = benchmarkLeaseList{
		field:   "clear_force_stopped",
		settled: "benchmark: cleared force_stopped overrides that a benchmark left behind",
		failed:  "benchmark: could not clear force_stopped overrides that a benchmark left behind; they may still be force_stopped, so clear them in the runtime section",
	}
	benchmarkLeaseRepins = benchmarkLeaseList{
		field:   "repin",
		settled: "benchmark: pinned launch specs again that a benchmark left unpinned",
		failed:  "benchmark: could not pin launch specs again that a benchmark left unpinned; they may still be unpinned, so pin them in the runtime section",
	}
)

// ReconcileBenchmarkOverrideLeases settles every leftover override lease at
// gateway start, server by server in id order. buildGatewayServer calls it
// before the benchmark scheduler starts and before the listeners do, so no run
// and no operator write can interleave with it, and no agent is connected to
// see the order of its writes: an agent gets the final document when it
// connects or polls. A read error leaves every row as it is.
func (s *Server) ReconcileBenchmarkOverrideLeases(ctx context.Context) {
	leases, err := s.Portal.BenchmarkOverrideLeases(ctx)
	if err != nil {
		slog.Warn("benchmark: could not read the override leases; leftover overrides are not reconciled at this start", "err", err)
		return
	}
	for _, serverID := range slices.Sorted(maps.Keys(leases)) {
		s.reconcileBenchmarkOverrideLease(ctx, serverID, leases[serverID])
	}
}

// reconcileBenchmarkOverrideLease settles one server's leftover lease. It
// clears the force_stopped overrides the lease names, then pins again the
// specs it names, each batch by compare-and-set against exactly the value a
// benchmark wrote (portal.SetBenchmarkRuntimeSpecsAdminState and
// SetBenchmarkRuntimeSpecsPinned, which notify the server's agent). The clear
// runs first: a spec that stays force_stopped refuses every request, while a
// spec that stays unpinned still serves on demand. A spec that was never
// written, or that has changed since, is a conflict and is left alone, and a
// deleted or retyped spec is gone; neither stays in the lease.
//
// left is what it could not settle (a failed write), and the row is rewritten
// to it, which releases the row when it is empty. When that rewrite fails, the
// row keeps the lease it had, and the next reconcile's compare-and-set skips
// whatever is settled by then.
//
// It runs on a context of its own, bounded by benchmarkLeaseReconcileTimeout
// and not cancelled with its caller, because its writes are restores.
func (s *Server) reconcileBenchmarkOverrideLease(ctx context.Context, serverID string, lease portal.BenchmarkOverrideLease) (left portal.BenchmarkOverrideLease) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), benchmarkLeaseReconcileTimeout)
	defer cancel()
	out, err := s.Portal.SetBenchmarkRuntimeSpecsAdminState(rctx, lease.ClearForceStopped, vramAdminStateForceStopped, "")
	left.ClearForceStopped = logBenchmarkLeaseOutcome(serverID, benchmarkLeaseClears, out, err)
	out, err = s.Portal.SetBenchmarkRuntimeSpecsPinned(rctx, lease.Repin, false, true)
	left.Repin = logBenchmarkLeaseOutcome(serverID, benchmarkLeaseRepins, out, err)
	if err := s.Portal.SetBenchmarkOverrideLease(rctx, serverID, left); err != nil {
		slog.Warn("benchmark: could not rewrite the override lease after a reconcile; the row keeps what it named", "server_id", serverID, "err", err)
	}
	return left
}

// logBenchmarkLeaseOutcome logs what one reconcile batch did and returns what
// it could not settle: the failed specs, which the row keeps. A failed write
// may still have stored its row, and the next reconcile's compare-and-set
// then finds a conflict. A conflicting spec leaves the lease because its
// stored value is not the one a benchmark wrote (an operator changed it since,
// or the dead run never wrote it), so nothing is left to undo. A gone spec
// leaves it because no benchmark write can reach it again: a deleted spec has
// no row, and a retyped one (its application is no longer server_agent) keeps
// its stored value, which the portal names in a Warn of its own and which acts
// again only if the application is retyped back.
func logBenchmarkLeaseOutcome(serverID string, list benchmarkLeaseList, out portal.BenchmarkSpecsOutcome, err error) []string {
	if len(out.Written) > 0 {
		slog.Warn(list.settled, "server_id", serverID, "spec_ids", out.Written)
	}
	if len(out.Gone) > 0 || len(out.Conflict) > 0 {
		slog.Info("benchmark: dropped override lease entries that no longer apply", "server_id", serverID, "list", list.field, "gone", out.Gone, "conflict", out.Conflict)
	}
	if len(out.Failed) > 0 {
		slog.Warn(list.failed, "server_id", serverID, "spec_ids", out.Failed, "err", err)
	}
	return out.Failed
}

// benchmarkAdminStateForceRunning is the admin override that keeps a spec
// running whatever else happens. A spec carrying it restarts at once after any
// stop, so beginBenchmarkUnpin turns every stop off when one is on the server.
// A value of portal's closed admin_state set, mirrored here like
// vramAdminStateForceStopped.
const benchmarkAdminStateForceRunning = "force_running"

// benchmarkOverrides is a manual speed or both run's override state on its
// server. runBenchmark owns it, and only the run's goroutine touches it.
type benchmarkOverrides struct {
	run          *benchmarkRun // the run whose status names the lists; preStopServer publishes through it
	stopsAllowed bool          // an agent target may stop the server's running models (preStopServer)
	leaseWritten bool          // the run wrote the lease row at least once; endBenchmarkUnpin then rewrites it
	// unpinned is what the run owes a re-pin and what the lease's repin names,
	// sorted: the whole pinned set from just before the unpin batch, then the
	// specs the batch wrote, or, after a batch that failed, the specs its
	// immediate re-pin could not pin again, which can include one whose unpin
	// was never stored; after the run's final re-pin, the specs it could not
	// pin.
	unpinned []string
	stopOwed []string // force_stopped written or possibly written and a clear owed, sorted
	stopped  []string // the audit set: every spec a stop batch wrote or may have written, sorted
}

// writeBenchmarkLease rewrites the whole row from the run's state:
// repin = ov.unpinned, clear_force_stopped = ov.stopOwed. A row with both lists
// empty is released. It sets ov.leaseWritten on every attempt, successful or
// not, because a failed write may still have stored the row, and a row the run
// may have touched is always rewritten at its end (endBenchmarkUnpin).
//
// The lease row is not an input of the runtime-config document, so this write
// owes no notification.
func (s *Server) writeBenchmarkLease(ctx context.Context, serverID string, ov *benchmarkOverrides) error {
	ov.leaseWritten = true
	return s.Portal.SetBenchmarkOverrideLease(ctx, serverID, portal.BenchmarkOverrideLease{Repin: ov.unpinned, ClearForceStopped: ov.stopOwed})
}

// beginBenchmarkUnpin decides, once per run and before the first target,
// whether the run's agent targets may stop the server's running models
// (benchmarkOverrides.stopsAllowed), and lifts the server's pins for the run.
// It always returns one non-nil state, and a state whose stopsAllowed is still
// false turns every stop off for the run. Its gates, in order:
//  0. the run has an agent target with mayPreStop at all; a scheduled,
//     capacity or vision run, or one without an agent target, writes nothing;
//  1. the agent applies runtime configuration (vramIsolationUnavailable): in
//     file mode, or without runtime_manager, a write changes nothing;
//  2. the application's launch specs can be read;
//  3. no enabled spec carries force_running: such a spec restarts at once after
//     any stop and thrashes with a stopped target;
//  4. a lease an earlier run left on this server is settled first
//     (settleBenchmarkLeftoverLease): without the row's content, or with
//     something the reconcile could not settle, a lease write of this run
//     would overwrite what the row still names.
//
// Every gate that fails logs why and leaves stops off. Once they all pass, it
// reads the launch specs again if a reconcile ran, because a reconcile's
// re-pins change which specs are pinned, and lifts every enabled pinned spec
// of the application for the run, the measured ones included
// (unpinBenchmarkPins): the stop-all may stop only unpinned specs, because a
// pinned one restarts at the stop's clear. endBenchmarkUnpin pins them again
// after the whole run.
//
// It does not wait for anything: every runtime-config document for the server
// is derived and enqueued in write order, so a reconcile's documents reach the
// agent before the run's own, and the unpin's before any stop's.
func (s *Server) beginBenchmarkUnpin(ctx context.Context, run *benchmarkRun, serverID string, targets []benchmarkTarget) *benchmarkOverrides {
	ov := &benchmarkOverrides{run: run}
	agentApp, ok := benchmarkStopApplication(targets)
	if !ok {
		return ov
	}
	if reason, unavailable := s.vramIsolationUnavailable(ctx, serverID); unavailable {
		slog.Info("benchmark: running agent models are not stopped in this run", "server_id", serverID, "reason", reason)
		return ov
	}
	specs, err := s.Routes.RuntimeSpecsByApplication(ctx, agentApp.ID)
	if err != nil {
		slog.Warn("benchmark: could not read the launch specs; nothing is unpinned or stopped in this run", "server_id", serverID, "err", err)
		return ov
	}
	if specID, found := benchmarkForceRunningSpec(specs); found {
		slog.Info("benchmark: a launch spec carries force_running; nothing is unpinned or stopped in this run", "server_id", serverID, "spec_id", specID)
		return ov
	}
	mayWrite, reconciled := s.settleBenchmarkLeftoverLease(ctx, serverID)
	if !mayWrite {
		return ov
	}
	if reconciled {
		if specs, err = s.Routes.RuntimeSpecsByApplication(ctx, agentApp.ID); err != nil {
			slog.Warn("benchmark: could not read the launch specs; nothing is unpinned or stopped in this run", "server_id", serverID, "err", err)
			return ov
		}
	}
	s.unpinBenchmarkPins(ctx, run, serverID, ov, specs)
	return ov
}

// benchmarkStopApplication returns the application of the first target that
// may stop the server's running agent models (mayPreStop on a server_agent
// application). A run touches exactly one server, and a server has at most
// one server_agent application.
func benchmarkStopApplication(targets []benchmarkTarget) (routing.Application, bool) {
	for _, tgt := range targets {
		if tgt.mayPreStop && tgt.app.Type == routing.ProviderServerAgent {
			return tgt.app, true
		}
	}
	return routing.Application{}, false
}

// benchmarkForceRunningSpec returns the id of the first enabled spec that
// carries force_running.
func benchmarkForceRunningSpec(specs []routing.RuntimeSpec) (string, bool) {
	for _, spec := range specs {
		if spec.Enabled && strings.TrimSpace(spec.AdminState) == benchmarkAdminStateForceRunning {
			return spec.ID, true
		}
	}
	return "", false
}

// settleBenchmarkLeftoverLease reads serverID's override lease and settles a
// leftover one (reconcileBenchmarkOverrideLease): an earlier run in this
// process could not release its row, a VRAM run's restore failed, or a crashed
// process's row survived a failed start-up reconcile. mayWrite reports whether
// the run may write a lease of its own: false when the read failed, because
// the run could not rewrite the row without losing what it names, and false
// when the reconcile left something behind, which only the row now holds.
// reconciled reports that a leftover was reconciled at all: its re-pins change
// which specs are pinned, so the caller reads them again.
func (s *Server) settleBenchmarkLeftoverLease(ctx context.Context, serverID string) (mayWrite, reconciled bool) {
	leases, err := s.Portal.BenchmarkOverrideLeases(ctx)
	if err != nil {
		slog.Warn("benchmark: could not read the override lease; nothing is unpinned or stopped in this run", "server_id", serverID, "err", err)
		return false, false
	}
	lease := leases[serverID]
	if lease.Empty() {
		return true, false
	}
	if left := s.reconcileBenchmarkOverrideLease(ctx, serverID, lease); !left.Empty() {
		slog.Info("benchmark: an earlier benchmark's override lease is still owed; nothing is unpinned or stopped in this run", "server_id", serverID)
		return false, true
	}
	return true, true
}

// unpinBenchmarkPins lifts every enabled pinned spec in specs for the run: the
// last steps of beginBenchmarkUnpin, on the launch specs it read last.
//   - Nothing pinned: nothing is written, and stops are allowed.
//   - The override lease names the whole pinned set before any spec is
//     written, so a process that dies after the batch leaves a record that the
//     reconciler pins again. A failed lease write unpins and stops nothing.
//   - One batch unpins the set, with one notification, after a Warn that names
//     it: the trace of last resort after a crash.
//   - A batch that failed for any spec is undone at once (undoBenchmarkUnpin),
//     and stops stay off.
//   - A batch that wrote fewer specs than the lease names rewrites the lease
//     to what it wrote, which releases it when that is nothing; stops are
//     allowed. The written specs are what the run owes a re-pin, and the
//     status names them.
func (s *Server) unpinBenchmarkPins(ctx context.Context, run *benchmarkRun, serverID string, ov *benchmarkOverrides, specs []routing.RuntimeSpec) {
	pinnedSet := benchmarkPinnedSet(specs)
	if len(pinnedSet) == 0 {
		ov.stopsAllowed = true
		return
	}
	ov.unpinned = pinnedSet
	if err := s.writeBenchmarkLease(ctx, serverID, ov); err != nil {
		ov.unpinned = nil
		slog.Warn("benchmark: could not record the override lease; nothing is unpinned or stopped in this run", "server_id", serverID, "err", err)
		return
	}
	slog.Warn("benchmark: unpinning the server's pinned launch specs for the run; they are pinned again when it ends", "server_id", serverID, "spec_ids", pinnedSet)
	out, err := s.Portal.SetBenchmarkRuntimeSpecsPinned(ctx, pinnedSet, true, false)
	if err != nil {
		s.undoBenchmarkUnpin(ctx, run, serverID, ov, out, err)
		return
	}
	ov.unpinned = out.Written
	ov.stopsAllowed = true
	// A spec the batch found changed or gone owes no re-pin, and a reconcile
	// that pinned a changed one again would undo an operator's write. A lease
	// with both lists empty is released.
	if len(out.Written) < len(pinnedSet) {
		if lerr := s.writeBenchmarkLease(ctx, serverID, ov); lerr != nil {
			slog.Warn("benchmark: could not update the override lease after the unpin; it is rewritten when the run ends", "server_id", serverID, "err", lerr)
		}
	}
	if len(out.Written) > 0 {
		run.setUnpinned(out.Written)
		s.Benchmarks.publish(serverID, run.snapshot())
	}
}

// benchmarkPinnedSet is the sorted ids of the enabled specs that are pinned:
// what a manual speed run unpins for its duration. A disabled spec is not in
// the runtime-config document, so its pin acts on nothing.
func benchmarkPinnedSet(specs []routing.RuntimeSpec) []string {
	var ids []string
	for _, spec := range specs {
		if spec.Enabled && spec.Pinned {
			ids = append(ids, spec.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// undoBenchmarkUnpin settles an unpin batch that failed for at least one spec.
// It pins the written and the failed specs again at once, on a context that
// the run's cancellation does not reach. A failed spec is included because its
// write may have stored pinned = false before its GPU-row write failed; for a
// spec that is still pinned, the compare-and-set is a harmless conflict. What
// stays unpinned is what the run still owes a re-pin: the lease is rewritten to
// it, which releases the lease when nothing is owed, the status names it, and
// endBenchmarkUnpin retries it. Stops stay off for the run.
func (s *Server) undoBenchmarkUnpin(ctx context.Context, run *benchmarkRun, serverID string, ov *benchmarkOverrides, out portal.BenchmarkSpecsOutcome, err error) {
	slog.Warn("benchmark: could not unpin every pinned launch spec; running models are not stopped in this run", "server_id", serverID, "err", err)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vramRestoreTimeout)
	defer cancel()
	restore, _ := s.Portal.SetBenchmarkRuntimeSpecsPinned(rctx, benchmarkUnion(out.Written, out.Failed), false, true)
	ov.unpinned = restore.Failed
	if lerr := s.writeBenchmarkLease(rctx, serverID, ov); lerr != nil {
		slog.Warn("benchmark: could not update the override lease after the unpin; it is rewritten when the run ends", "server_id", serverID, "err", lerr)
	}
	if len(restore.Failed) > 0 {
		run.setUnpinned(restore.Failed)
		s.Benchmarks.publish(serverID, run.snapshot())
	}
}

// endBenchmarkUnpin runs when the run ends, registered right after
// beginBenchmarkUnpin, so before the run's finish defer and while the run still
// holds the reservation: operator writes to the server's launch specs stay
// refused until the re-pin is written. It works on a context that the run's
// cancellation does not reach.
//   - Every spec the run still owes a re-pin is pinned again in one batch, and
//     logBenchmarkRepin logs what the batch did. A spec that is gone took its
//     pin with it, and one that is pinned again already needs nothing.
//   - Whenever the run wrote the lease row, it rewrites it from the run's final
//     state, which releases it when nothing is owed any more. A run that
//     stopped something writes the row before each stop batch and after each
//     clear; the write after a clear can fail on a store error and leave the
//     stop set named in the row, which a later reconcile would then clear,
//     possibly over an operator's own later force_stopped. So the rewrite does
//     not depend on whether the run unpinned anything.
//   - A spec whose re-pin failed stays in the lease for the reconciler, and the
//     status and the run's error name it.
func (s *Server) endBenchmarkUnpin(ctx context.Context, run *benchmarkRun, serverID string, ov *benchmarkOverrides, runErr *string) {
	if len(ov.unpinned) == 0 && !ov.leaseWritten {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vramRestoreTimeout)
	defer cancel()
	var out portal.BenchmarkSpecsOutcome
	if len(ov.unpinned) > 0 {
		out, _ = s.Portal.SetBenchmarkRuntimeSpecsPinned(rctx, ov.unpinned, false, true)
		logBenchmarkRepin(serverID, out)
		ov.unpinned = out.Failed
	}
	if err := s.writeBenchmarkLease(rctx, serverID, ov); err != nil {
		slog.Warn("benchmark: could not rewrite the override lease after the run", "server_id", serverID, "err", err)
	}
	if len(out.Failed) > 0 {
		run.setRepinFailed(out.Failed)
		msg := fmt.Sprintf("launch specs may still be unpinned after the benchmark: %s; pin them in the runtime section", strings.Join(out.Failed, ", "))
		*runErr = benchmarkJoinRunError(*runErr, msg)
	}
}

// logBenchmarkRepin logs what the re-pin at a run's end did to each spec.
func logBenchmarkRepin(serverID string, out portal.BenchmarkSpecsOutcome) {
	if len(out.Written) > 0 {
		slog.Info("benchmark: pinned launch specs again after the run", "server_id", serverID, "spec_ids", out.Written)
	}
	for _, id := range out.Conflict {
		slog.Info("benchmark: a launch spec was already pinned again when the run ended", "server_id", serverID, "spec_id", id)
	}
	for _, id := range out.Failed {
		slog.Warn("benchmark: could not pin a launch spec again after the run; pin it in the runtime section", "server_id", serverID, "spec_id", id, "err", out.Errs[id])
	}
}

// benchmarkJoinRunError appends msg to a run's error text, after "; " when
// the run already has one.
func benchmarkJoinRunError(runErr, msg string) string {
	if runErr == "" {
		return msg
	}
	return runErr + "; " + msg
}
