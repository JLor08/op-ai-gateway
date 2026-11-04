// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"strconv"
	"strings"
)

// instanceSeparator joins a base name and an instance index, in an instance
// address ("m#2") and in a gateway instance key ("map_x#2") alike.
const instanceSeparator = "#"

// InstanceAddress returns the upstream model name that reaches instance k of a
// replicable spec whose upstream model is base: base itself for k == 0 -- the
// address every mapping has always sent -- and "base#k" for k >= 1. It is the
// agent's rule (server-agent/internal/runtime.InstanceAddress), which this
// module cannot import, so both spell it.
func InstanceAddress(base string, k int) string {
	if k == 0 {
		return base
	}
	return base + instanceSeparator + strconv.Itoa(k)
}

// InstanceBase is InstanceAddress's inverse for a known k: the base name of
// address when address is instance k's address of it, and address unchanged
// otherwise. It strips only the suffix k names, so a base that itself contains
// a '#' ("foo#2" at instance 0) comes back whole.
func InstanceBase(address string, k int) string {
	if k < 1 {
		return address
	}
	return strings.TrimSuffix(address, instanceSeparator+strconv.Itoa(k))
}

// InstanceKey returns the gateway's key for instance k of the mapping
// mappingID: "<mapping_id>#<k>" for every k, instance 0 included, so a key
// always names its instance. It keys what is held per instance; RouteID stays
// the mapping id, which everything attributed per mapping keys by.
func InstanceKey(mappingID string, k int) string {
	return mappingID + instanceSeparator + strconv.Itoa(k)
}

// Instance returns the instance index this target addresses, read from its
// InstanceKey: 0 for instance 0 and for a target without a key (a vendor
// target, or one built by hand).
func (t Target) Instance() int {
	i := strings.LastIndex(t.InstanceKey, instanceSeparator)
	if i < 0 {
		return 0
	}
	// Only the canonical decimal InstanceKey writes is an index: no sign, no
	// leading zero, within the address range.
	suffix := t.InstanceKey[i+len(instanceSeparator):]
	k, err := strconv.Atoi(suffix)
	if err != nil || k < 0 || k > MaxInstanceIndex || strconv.Itoa(k) != suffix {
		return 0
	}
	return k
}

// InstanceView is what the gateway knows about the instances of one mapping's
// replicable spec on one server.
type InstanceView struct {
	Replicable bool
	// Parallelism is how many requests one instance serves at once (the
	// spec's instance_parallelism, 1 when unset).
	Parallelism int
	// MaxInstances is the spec's max_instances (0 = no cap of its own).
	MaxInstances int
	// Running lists the instance indexes k >= 1 the agent reports running.
	Running []int
	// Present lists the instance indexes k >= 1 the agent reports starting
	// or running.
	Present []int
}

// InstanceSource reports the instances of mapping m on serverID. ok is false
// when the mapping is not instance-capable there: no runtime spec, a spec that
// is not replicable, or an agent not known to run instances. ctx is the
// request's, so a store read the source makes is traced under the request and
// ends with it. Satisfied by the gateway's adapter over its runtime-status and
// agent-feature registries.
type InstanceSource interface {
	InstanceView(ctx context.Context, serverID string, m ModelMapping) (InstanceView, bool)
}

// InstanceActivityChecker reports how many requests are in flight on one
// instance, by its gateway instance key (InstanceKey). Satisfied by the
// gateway's active-request registry.
type InstanceActivityChecker interface {
	InstanceActivity(instanceKey string) int
}

// SetInstanceActivityChecker installs the per-instance in-flight count the
// capacity filter judges instance candidates by. Leaving it unset (nil) means
// an instance's load is unknown, so no instance candidate is ever at capacity
// -- the same lenience a nil ServerActivityChecker gives the server cap.
func (r *Resolver) SetInstanceActivityChecker(c InstanceActivityChecker) { r.instanceActivity = c }

// DemandSignal says a gateway model needs another instance: a request is
// about to wait in the admission queue -- or, one that must not wait
// (inference.Request.NoPark) or a pinned one (which waits only when it needs a
// free live slot and cannot be served without one), has just been served on a
// busy candidate -- because every candidate it could use was at capacity, at
// least one of them an instance.
type DemandSignal struct {
	// Model is the gateway model whose instances were all busy -- a group
	// member's own name, never the group's.
	Model string
	// APIFlavor is the coarse flavor (NormalizeAPIFlavor) the model was
	// resolved under, the one ActiveMappingsForModel keys by.
	APIFlavor string
	// ServerID restricts the new instance to one server -- a pinned
	// request's (a token's affinity pin, a server override), which no other
	// server can serve; "" = any server.
	ServerID string
	// Live is true when the demand is for a live slot (a live session).
	Live bool
	// MappingIDs are the instance-capable mappings of Model the waiting
	// request could use -- the ones that passed its provisioning, endpoint,
	// capability and reachability filters -- once each, in candidate order. An
	// instance started for any other mapping of Model would not serve it.
	MappingIDs []string
}

// DemandSink receives demand signals. Signal is called on the request path,
// from as many goroutines as there are requests parking, once per park turn
// per at-capacity member with instance candidates: a plain model is its own
// one member, and a group request parked on two members whose instances are
// all busy signals for each, by its own model. So a request that waits
// through several turns signals several times, and a sink that wants one
// scale-up per need coalesces them. It is called only for a request that
// actually parks: not on the fail-open path without a queue, not when the
// request's admission deadline has passed, and not when the queue has no
// room for it (AdmissionRoom) -- and once for a request that must not park
// (inference.Request.NoPark), after it was served on a busy candidate where
// it would have parked: for a group, for the walk's first at-capacity member
// with instance candidates, which need not be the member that served it. A
// pinned request served on a busy instance of its pinned server signals once
// too, restricted to that server, and a parked pinned request once per park
// turn. A request that could not be served at all signals nothing, and neither does
// a check-only resolve (inference.Request.CheckOnly). It must not block: the
// request is about to wait, or to be served, and a sink that blocked would
// hold it there.
type DemandSink interface {
	Signal(DemandSignal)
}

// LiveSlotChecker reports whether an instance's live slot is held: a live
// transcription session is in progress on it, by its gateway instance key
// (InstanceKey). Satisfied by the gateway's live-session registry.
type LiveSlotChecker interface {
	LiveHeld(instanceKey string) bool
}

// SetLiveSlotChecker installs the live-slot source that the resolve of a
// request needing a free live slot -- a live session
// (inference.Request.LiveSession) and every transcription, buffered or not --
// judges its candidates by, in a pool that holds an instance candidate: the
// instances, and a mixed fleet's mappings without instances beside them. Leaving
// it unset (nil) means no slot is known to be held: such a request is routed as
// any request is, and only a session's own slot acquire, or a buffered
// transcription's held check, can find the slot taken.
func (r *Resolver) SetLiveSlotChecker(c LiveSlotChecker) { r.liveSlots = c }

// SetDemandSink installs the receiver of demand signals. Leaving it unset
// (nil) drops them: an all-busy request still queues, nothing starts another
// instance for it.
func (r *Resolver) SetDemandSink(s DemandSink) { r.demand = s }

// SetInstanceSource installs the instance source candidate expansion reads.
// Leaving it unset (nil) means no mapping is instance-capable, so every
// candidate is its mapping's instance 0 -- the address it has always had.
func (r *Resolver) SetInstanceSource(s InstanceSource) { r.instances = s }

// expandInstances returns cands with each instance-capable candidate followed
// by one candidate per instance k >= 1 its agent reports running, in the
// order the source lists them; every other candidate is passed through as the
// instance 0 it is. A starting instance is not a candidate: a request routed
// to it would wait out its start, while one parked in the admission queue
// takes whichever instance frees first and sees the new one once it runs.
//
// Only a server_agent application has an agent to run instances, so no other
// candidate is asked about; that also keeps the source's spec lookup off every
// request for a model served without an agent. An index no address can carry,
// or one the source repeats, adds nothing.
//
// Every candidate of a capable mapping, instance 0 included, carries the
// view's facts (InstanceCapable, InstanceParallelism): the source is asked
// once per mapping per call, and the capacity filter judges the candidates by
// the very view that produced them.
func expandInstances(ctx context.Context, cands []MappingCandidate, src InstanceSource) []MappingCandidate {
	if src == nil {
		return cands
	}
	out := make([]MappingCandidate, 0, len(cands))
	for _, c := range cands {
		if c.Application.Type != ProviderServerAgent {
			out = append(out, c)
			continue
		}
		view, ok := src.InstanceView(ctx, c.Server.ID, c.Mapping)
		if !ok {
			out = append(out, c)
			continue
		}
		c.InstanceCapable = true
		// An unset parallelism is one request at a time; never 0, which
		// would leave the instance at capacity with nothing in flight.
		c.InstanceParallelism = max(view.Parallelism, 1)
		out = append(out, c)
		seen := make(map[int]struct{}, len(view.Running))
		for _, k := range view.Running {
			if k < 1 || k > MaxInstanceIndex {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			inst := c
			inst.Instance = k
			out = append(out, inst)
		}
	}
	return out
}
