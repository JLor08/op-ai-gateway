// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"fmt"
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

// isCommand marks cmdStop as an owner command; the marker has no body.
func (cmdStop) isCommand() {
	// Intentionally empty: it only places cmdStop in the command set.
}

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
//   - StopStopping for an instance already draining, whatever its siblings do:
//     it is on its way out already;
//   - ErrInstanceNotHighest while a sibling with a higher index has a process;
//   - StopStopping otherwise: the drain began.
//
// The checks run in that order, so a refusal wins over the idempotent
// answers: a protected instance is refused even with no process, because the
// agent is about to start it again on its own and "stopped" would not be true
// for long.
//
// Every stop that is not refused also answers the requests queued on the
// instance at that moment (stopQueuedError): the ensure a starting instance is
// loading for, or a request waiting out a crash-loop backoff or an admission.
// Left queued, they would want the instance up again the moment its process
// exits, so a stop of a starting instance would load the model a second time
// and leave it running. A request that arrives after the stop is ordinary
// demand and may start the instance again.
//
// What follows the drain is the ordinary exit path (onProcExited): a burst
// instance with nothing left to do is retired, its row leaving Status(); any
// other instance rests at stopped. A request queued on instance 0 while it
// drains starts it again when it exits -- the normal demand behaviour, which
// the stop does not suspend. A burst instance queues nothing while it drains
// (handleEnsure refuses it at once), so it is always retired.
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
		if st != nil {
			o.failPending(st, stopQueuedError(st))
		}
		return stopOutcome{state: StopStopped}
	}
	if st.state != StateDraining && o.higherSiblingRuns(st) {
		return stopOutcome{err: ErrInstanceNotHighest}
	}
	o.failPending(st, stopQueuedError(st))
	o.beginDrain(key) // a no-op for an instance already draining
	return stopOutcome{state: StopStopping}
}

// stopQueuedError is what a stop answers each request queued on st. The codes
// are the ones the gateway already acts on, wrapped with a message that names
// the stop:
//
//   - a burst instance's request gets runtime.instance_no_capacity (409), the
//     refusal of a burst instance that is not serving: the gateway places a
//     plain request on another instance, and a manual start or a scaler ensure
//     reports the refusal;
//   - instance 0's request (the only unprotected instance that is not burst)
//     gets runtime.admission_blocked (503), "the agent will not admit it now":
//     a later request may start it again, and the gateway does not resend a
//     request that failed with it.
func stopQueuedError(st *specState) error {
	if st.burst() {
		return fmt.Errorf("%w: instance %s was stopped", ErrInstanceNoCapacity, st.spec.ID)
	}
	return fmt.Errorf("%w: instance %s was stopped", ErrAdmissionBlocked, st.spec.ID)
}
