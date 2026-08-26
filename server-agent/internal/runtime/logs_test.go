// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strings"
	"sync"
	"testing"
)

// TestRingBufferTail covers the empty buffer, a short write (no truncation),
// exact-capacity, over-capacity (oldest bytes dropped), and n<=0 / n larger
// than the buffer.
func TestRingBufferTail(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		var r ringBuffer
		if got := r.Tail(10); got != "" {
			t.Errorf("Tail on empty buffer = %q, want \"\"", got)
		}
	})

	t.Run("short write, no truncation", func(t *testing.T) {
		var r ringBuffer
		if _, err := r.Write([]byte("hello world")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := r.Tail(5); got != "world" {
			t.Errorf("Tail(5) = %q, want %q", got, "world")
		}
		if got := r.Tail(100); got != "hello world" {
			t.Errorf("Tail(100) (n > len) = %q, want %q", got, "hello world")
		}
	})

	t.Run("n<=0 returns empty", func(t *testing.T) {
		var r ringBuffer
		r.Write([]byte("data"))
		if got := r.Tail(0); got != "" {
			t.Errorf("Tail(0) = %q, want \"\"", got)
		}
		if got := r.Tail(-1); got != "" {
			t.Errorf("Tail(-1) = %q, want \"\"", got)
		}
	})

	t.Run("over capacity drops oldest bytes", func(t *testing.T) {
		var r ringBuffer
		// Write well over ringBufferCap so the oldest content is guaranteed
		// dropped, then confirm the buffer never exceeds capacity and Tail
		// returns exactly the most-recently-written suffix.
		chunk := strings.Repeat("a", 1024)
		total := 0
		for i := 0; i < 80; i++ { // 80 * 1024 = 80 KiB > 64 KiB cap
			r.Write([]byte(chunk))
			total += len(chunk)
		}
		r.mu.Lock()
		bufLen := len(r.buf)
		r.mu.Unlock()
		if bufLen > ringBufferCap {
			t.Fatalf("internal buffer length = %d, want <= %d", bufLen, ringBufferCap)
		}
		if bufLen != ringBufferCap {
			t.Fatalf("internal buffer length = %d, want exactly %d after overflowing writes", bufLen, ringBufferCap)
		}

		marker := "END-OF-STREAM-MARKER"
		r.Write([]byte(marker))
		tail := r.Tail(len(marker))
		if tail != marker {
			t.Errorf("Tail after overflow = %q, want %q", tail, marker)
		}
		full := r.Tail(ringBufferCap + 1000) // request more than capacity
		if len(full) > ringBufferCap {
			t.Errorf("Tail returned %d bytes, want <= cap %d", len(full), ringBufferCap)
		}
		if !strings.HasSuffix(full, marker) {
			t.Errorf("Tail(large) does not end with the last write")
		}
	})

	t.Run("concurrent stdout+stderr writers are race-safe", func(t *testing.T) {
		var r ringBuffer
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					r.Write([]byte("x"))
				}
			}(g)
		}
		done := make(chan struct{})
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					r.Tail(16)
				}
			}
		}()
		wg.Wait()
		close(done)
	})
}
