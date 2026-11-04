// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"sync"
)

// The instance index rule, shared by the instance scaler (instance_scaler.go)
// and the portal's manual start (runtime_actions.go): an instance of a spec
// gets the lowest index that is
//   - not reported by the agent in a state that keeps the index
//     (specInstanceRows: a draining or stopped burst row is not free, one
//     resting at a failure is);
//   - not the address of another mapping's literal upstream model on the same
//     agent (otherMappingIndexes): the agent resolves a literal name before an
//     instance address, so that ensure would start the other spec;
//   - not held by an ensure under way (instanceClaims), whichever of the two
//     callers started it.
//
// The claim set is what keeps the two from asking for one index at the same
// time: an index is claimed before its ensure is sent and held until routing
// sees the instance (or the ensure failed), so neither caller hands it out a
// second time while the agent's status does not report it yet.

// instanceClaims is the set of instance indexes an ensure is under way for,
// per (server, mapping). One set is shared by the scaler and the manual start.
// A nil set holds nothing: every claim succeeds and nothing is remembered, the
// behaviour of a bare Server built without New.
type instanceClaims struct {
	mu   sync.Mutex
	held map[scaleKey]map[int]bool
}

func newInstanceClaims() *instanceClaims {
	return &instanceClaims{held: map[scaleKey]map[int]bool{}}
}

// heldBy returns a copy of the indexes held for key.
func (c *instanceClaims) heldBy(key scaleKey) map[int]bool {
	out := map[int]bool{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.held[key] {
		out[k] = true
	}
	return out
}

// tryClaim claims index k for key and reports whether it was free of a claim.
func (c *instanceClaims) tryClaim(key scaleKey, k int) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held[key][k] {
		return false
	}
	c.hold(key, k)
	return true
}

// claimLowest claims, in one step, the lowest index k with from <= k < limit
// that is neither in taken nor held for key, and returns it; ok is false when
// there is none. limit is capped at the highest index an address can carry.
func (c *instanceClaims) claimLowest(key scaleKey, from, limit int, taken map[int]bool) (int, bool) {
	limit = min(limit, routing.MaxInstanceIndex+1)
	if c == nil {
		for k := max(from, 0); k < limit; k++ {
			if !taken[k] {
				return k, true
			}
		}
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := max(from, 0); k < limit; k++ {
		if !taken[k] && !c.held[key][k] {
			c.hold(key, k)
			return k, true
		}
	}
	return 0, false
}

// hold records k for key; the caller holds c.mu.
func (c *instanceClaims) hold(key scaleKey, k int) {
	set, ok := c.held[key]
	if !ok {
		set = map[int]bool{}
		c.held[key] = set
	}
	set[k] = true
}

// release drops the claim on k for key, and key itself once nothing is held
// for it, so the set holds no more than the ensures under way.
func (c *instanceClaims) release(key scaleKey, k int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held[key], k)
	if len(c.held[key]) == 0 {
		delete(c.held, key)
	}
}

// specInstanceRows reads the status rows of spec specID: whether any instance
// of it is starting, instance 0 included, and every index k >= 1 the agent
// reports for it in a state that keeps the index taken. An index the agent
// still reports draining, stopped or waiting is not one an ensure can start
// afresh. An index resting at a failure (instanceReusableStates) is: the agent
// keeps that row only for the operator to read, until it is named again or
// the next Apply, and an ensure of it is exactly what names it again -- it
// retires a crashed or start_failed row and starts a fresh instance there, and
// runs admission on a not_permitted one again (behind the agent's refusal rate
// limit). Routing never names such a row, so were its index counted as taken
// nothing would ever free it, and every failure would use up one index for
// good -- the last one below max_instances included.
func specInstanceRows(rows []RuntimeStatusDTO, specID string) (starting bool, reported []int) {
	for i := range rows {
		row := &rows[i] // read-only: the published snapshot itself
		if row.SpecID != specID {
			continue
		}
		if row.State == agentSpecStateStarting {
			starting = true
		}
		if row.Instance >= 1 && !instanceReusableStates[row.State] {
			reported = append(reported, row.Instance)
		}
	}
	return starting, reported
}

// instanceReusableStates are the states a burst instance rests at after a
// failure, which the agent keeps until the index is named again: an ensure of
// such an index is the retry, so the index rule treats it as free.
var instanceReusableStates = map[string]bool{"crashed": true, "start_failed": true, "not_permitted": true}

// otherMappingIndexes returns every index k >= 1 whose address of m's upstream
// model is the literal upstream model of another of mappings -- the agent's
// specs, the mappings of the server's server_agent application.
func otherMappingIndexes(mappings []routing.ModelMapping, m routing.ModelMapping) map[int]bool {
	others := make(map[string]bool, len(mappings))
	for _, o := range mappings {
		if o.ID != m.ID {
			others[o.AppModelName] = true
		}
	}
	taken := map[int]bool{}
	for k := 1; k <= routing.MaxInstanceIndex && len(others) > 0; k++ {
		if others[routing.InstanceAddress(m.AppModelName, k)] {
			taken[k] = true
		}
	}
	return taken
}
