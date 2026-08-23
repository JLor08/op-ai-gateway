// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import "sync"

// Broker is a tiny in-process fan-out for "a usage row was written" signals.
// Signals carry no payload: a subscriber only learns that something changed and
// re-fetches scope-correctly, so no data crosses user boundaries through the broker.
type Broker struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func NewBroker() *Broker {
	return &Broker{subs: make(map[chan struct{}]struct{})}
}

// Register returns a new subscriber channel. It is buffered with a single slot so
// Publish never blocks: a pending signal already means "something changed".
func (b *Broker) Register() chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unregister detaches a subscriber. It intentionally does NOT close the channel:
// a closed channel is always readable and would spin the SSE handler's select loop.
func (b *Broker) Unregister(ch chan struct{}) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// Publish signals every current subscriber, dropping the signal for any whose buffer
// is already full. Never blocks. Safe against concurrent Register/Unregister.
func (b *Broker) Publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default: // buffer full: a signal is already pending, drop this one
		}
	}
}
