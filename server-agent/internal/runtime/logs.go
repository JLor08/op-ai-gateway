// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import "sync"

// ringBufferCap is the fixed capacity of a ringBuffer: enough for error
// context (the one "CUDA error: out of memory" line and its neighbors), not
// full log retention -- that is Task 3 (T3, log streaming)'s job, reserved
// separately.
const ringBufferCap = 64 * 1024

// ringBuffer is a fixed-capacity, last-N-bytes buffer over one process's
// combined stdout+stderr. It implements io.Writer so it can be assigned
// directly to exec.Cmd.Stdout AND exec.Cmd.Stderr: os/exec spawns one
// copying goroutine per stream when given a plain io.Writer, so Write must
// tolerate concurrent calls from both -- the mutex below is exactly that
// guard, never a performance nicety.
//
// Content captured here can contain fragments of whatever a chatty model
// server writes to its own stderr, which may include prompt text --
// treated as potentially prompt-bearing per the house logging rule this
// package follows: kept in memory, bounded, and NEVER written to disk (a
// caller must not persist Tail's output anywhere durable).
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// Write appends p, then drops the oldest bytes (if any) so the buffer never
// exceeds ringBufferCap. Never returns an error -- there is no failure mode
// for an in-memory bounded append.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if excess := len(r.buf) - ringBufferCap; excess > 0 {
		// Reallocate rather than reslice in place: reslicing would leave the
		// dropped prefix's backing array reachable (and its bytes never
		// zeroed), which is an unnecessary lingering copy of potentially
		// prompt-bearing content for a value this package promises to keep
		// bounded.
		trimmed := make([]byte, len(r.buf)-excess)
		copy(trimmed, r.buf[excess:])
		r.buf = trimmed
	}
	return len(p), nil
}

// Tail returns the last n bytes written (fewer if the buffer holds less),
// as a string. n<=0 returns "".
func (r *ringBuffer) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || len(r.buf) == 0 {
		return ""
	}
	if n > len(r.buf) {
		n = len(r.buf)
	}
	return string(r.buf[len(r.buf)-n:])
}
