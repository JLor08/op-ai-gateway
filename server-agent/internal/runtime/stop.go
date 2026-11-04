// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
)

// The transient stop of one instance: Manager.Stop, behind the router's
// POST /stop/{model}. It is the operator's "take this one down now" from the
// portal's live-status view, and it persists nothing -- no admin state, no pin,
// nothing in the runtime-config document -- so demand may start the instance
// again afterwards, exactly as after an idle unload. The persistent way to keep
// a spec down stays the pushed admin state (force_stopped).

// Typed refusals of a stop. Stable identity (errors.Is) and stable wire code
// (Error() text), like the sentinels in manager.go; the router maps both to
// 409 (sentinelCode).
var (
	// ErrInstanceProtected refuses a stop of an instance inside its spec's
	// protected floor (ProtectedCount: min_instances for a replicable spec, and
	// the one instance a pinned or force_running spec keeps). The agent wants
	// such an instance up with no request at all and would start it again at
	// once, so stopping it would only cost a model load. The way out is the
	// spec's own settings: clear the override, lower min_instances, or unpin.
	// Wire code runtime.instance_protected (409).
	ErrInstanceProtected = errors.New("runtime.instance_protected")
	// ErrInstanceNotHighest refuses a stop of an instance while a sibling with
	// a higher index still has a process (starting, running or draining): the
	// highest index leaves first (owner.higherSiblingRuns), the same ordering
	// the idle unload and every eviction keep, so the instances that run are
	// always the lowest ones. Wire code runtime.instance_not_highest (409).
	ErrInstanceNotHighest = errors.New("runtime.instance_not_highest")
)

// StopState is a successful stop's answer, the "state" of POST /stop/{model}'s
// body.
type StopState string

const (
	// StopStopping: the instance has a process, and a graceful drain of it has
	// begun or was already under way. In-flight requests finish (up to the
	// drain grace) before the process is signalled.
	StopStopping StopState = "stopping"
	// StopStopped: the instance has no process; there is nothing to stop. This
	// is what a repeated stop answers once the first one has finished, so the
	// call is idempotent.
	StopStopped StopState = "stopped"
)

// cmdStop asks the owner to stop one instance (Manager.Stop).
type cmdStop struct {
	model string
	reply chan stopOutcome
}

func (cmdStop) isCommand() {}

// stopOutcome is cmdStop's answer: a state, or one of the refusals.
type stopOutcome struct {
	state StopState
	err   error
}

// Stop begins a graceful drain of the instance model names -- an instance
// address, "m" for instance 0 or "m#k", resolved by the same rule as
// ResolveAddress -- and records nothing persistent. It answers at once; the
// drain itself runs on as usual (drain grace, then SIGTERM, then SIGKILL after
// the kill grace), and Status() reports its progress.
//
// Answers, decided in one owner turn, so no start or exit can slip in between
// the checks and the drain:
//
//   - ErrModelNotManaged: model is not a managed address (unknown, an index
//     the spec does not allow, or a spec the last push removed);
//   - StopStopping for any instance of a force_stopped spec that still has a
//     process: the push is draining it already, and the protected floor is
//     moot while the operator keeps the spec down; StopStopped for one with no
//     process;
//   - ErrInstanceProtected for an instance inside the protected floor
//     (ProtectedCount), with or without a process;
//   - StopStopped for an instance with no process (none created for an
//     allowed instance that does not exist): idempotent;
//   - ErrInstanceNotHighest while a sibling with a higher index has a process;
//   - StopStopping otherwise: the drain began, or was already running.
//
// What follows the drain is the ordinary exit path (onProcExited): a burst
// instance with nothing left to do is retired, its row leaving Status(); any
// other instance rests at stopped. A request queued on the instance when it
// exits starts it again -- the normal demand behaviour, which the stop does
// not suspend.
func (m *Manager) Stop(ctx context.Context, model string) (StopState, error) {
	reply := make(chan stopOutcome, 1)
	select {
	case m.cmds <- cmdStop{model: model, reply: reply}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-m.done:
		return "", ErrManagerClosed
	}
	// The owner answers in the very turn it reads the command, before it could
	// stop and close done, so a closed done never hides a sent answer; the
	// fallback only reads it first.
	select {
	case out := <-reply:
		return out.state, out.err
	case <-m.done:
		select {
		case out := <-reply:
			return out.state, out.err
		default:
			return "", ErrManagerClosed
		}
	}
}

// stopInstance is Manager.Stop on the owner (see its doc for the answers).
// beginDrain is the one drain path every other stop uses too (an idle unload,
// an eviction, a push), so a stopped instance leaves exactly as those do.
func (o *owner) stopInstance(model string) stopOutcome {
	if o.closing {
		return stopOutcome{err: ErrManagerClosed}
	}
	specID, _, k, ok := o.resolveAddress(model)
	if !ok {
		return stopOutcome{err: ErrModelNotManaged}
	}
	base := o.specs[specID]
	if base.removed {
		return stopOutcome{err: ErrModelNotManaged}
	}
	key := InstanceKey(base.spec.ID, k)
	st := o.specs[key]
	hasProc := st != nil && st.proc != nil
	if base.spec.AdminState == "force_stopped" {
		if !hasProc {
			return stopOutcome{state: StopStopped}
		}
		o.beginDrain(key) // a no-op when the push's own drain is under way
		return stopOutcome{state: StopStopping}
	}
	if k < ProtectedCount(base.spec) {
		return stopOutcome{err: ErrInstanceProtected}
	}
	if !hasProc {
		return stopOutcome{state: StopStopped}
	}
	if o.higherSiblingRuns(st) {
		return stopOutcome{err: ErrInstanceNotHighest}
	}
	o.beginDrain(key)
	return stopOutcome{state: StopStopping}
}
