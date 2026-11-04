// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
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
// is not replicable, or an agent not known to run instances. Satisfied by the
// gateway's adapter over its runtime-status and agent-feature registries.
type InstanceSource interface {
	InstanceView(serverID string, m ModelMapping) (InstanceView, bool)
}

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
func expandInstances(cands []MappingCandidate, src InstanceSource) []MappingCandidate {
	if src == nil {
		return cands
	}
	out := make([]MappingCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, c)
		if c.Application.Type != ProviderServerAgent {
			continue
		}
		view, ok := src.InstanceView(c.Server.ID, c.Mapping)
		if !ok {
			continue
		}
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
