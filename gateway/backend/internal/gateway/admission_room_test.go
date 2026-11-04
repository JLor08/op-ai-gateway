// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The queue has room while fewer waiters than its depth are parked, none once it
// is full -- exactly when its WaitForSlot refuses a waiter as full -- and room again
// once a waiter leaves. A nil queue has none.
func TestAdmissionQueueHasRoom(t *testing.T) {
	q := newAdmissionQueue(1)
	q.recheck = 0
	if !q.HasRoom() {
		t.Fatal("an empty queue: HasRoom = false, want true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.WaitForSlot(ctx, []string{"srv1"}, 0) }()
	waitForParked(t, q, 1)
	if q.HasRoom() {
		t.Fatal("a queue at its depth: HasRoom = true, want false")
	}
	if err := q.WaitForSlot(context.Background(), []string{"srv1"}, time.Second); !errors.Is(err, errAdmissionFull) {
		t.Fatalf("WaitForSlot on the full queue = %v, want errAdmissionFull, as HasRoom said", err)
	}
	cancel()
	<-done
	if !q.HasRoom() {
		t.Fatal("after the waiter left: HasRoom = false, want true")
	}
	var none *admissionQueue
	if none.HasRoom() {
		t.Fatal("a nil queue: HasRoom = true, want false")
	}
}
