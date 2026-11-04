// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "op-ai-gateway/internal/routing"

// The resolver asks the queue whether it has room before a parking request signals
// demand for another instance (routing.AdmissionRoom).
var _ routing.AdmissionRoom = (*admissionQueue)(nil)

// HasRoom reports whether WaitForSlot would take one more waiter now: fewer than
// maxDepth are parked. The resolver asks right before a request parks and signals
// demand, so a request the queue is about to refuse as full starts no instance. It is
// a snapshot -- the queue may fill or drain before the request's own WaitForSlot --
// and a nil queue has no room, as its WaitForSlot refuses every waiter as full.
func (q *admissionQueue) HasRoom() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters) < q.maxDepth
}
