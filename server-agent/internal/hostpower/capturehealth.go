// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import "time"

// captureWarnInterval rate-limits the warning for a read loop that keeps
// failing.
const captureWarnInterval = time.Minute

// captureHealth tracks whether the capture read loop works and mirrors it
// into the table, so the resolver fails closed while it does not.
type captureHealth struct {
	table    *ObservationTable
	failing  bool
	lastWarn time.Time
}

// ok records a working read (data or an idle timeout) and reports whether it
// ends a failure period.
func (h *captureHealth) ok() bool {
	h.table.setCaptureHealthy(true)
	recovered := h.failing
	h.failing = false
	return recovered
}

// failed records a read error at now and reports whether to log a warning:
// at the start of a failure period and at most once per captureWarnInterval
// while it lasts.
func (h *captureHealth) failed(now time.Time) bool {
	h.table.setCaptureHealthy(false)
	start := !h.failing
	h.failing = true
	if start || now.Sub(h.lastWarn) >= captureWarnInterval {
		h.lastWarn = now
		return true
	}
	return false
}
