// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"testing"
	"time"
)

func TestBrokerPublishReachesSubscribers(t *testing.T) {
	b := NewBroker()
	a := b.Register()
	c := b.Register()

	b.Publish()

	for i, ch := range []chan struct{}{a, c} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive publish", i)
		}
	}
}

func TestBrokerUnregisterStopsDelivery(t *testing.T) {
	b := NewBroker()
	ch := b.Register()
	b.Unregister(ch)

	b.Publish()

	select {
	case <-ch:
		t.Fatal("received publish after unregister")
	case <-time.After(100 * time.Millisecond):
		// no delivery: correct
	}
}

func TestBrokerPublishDoesNotBlockOnFullBuffer(t *testing.T) {
	b := NewBroker()
	_ = b.Register() // never drained: its 1-slot buffer fills on the first Publish

	done := make(chan struct{})
	go func() {
		b.Publish() // fills the buffer
		b.Publish() // buffer full -> must drop, not block
		b.Publish()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
}
